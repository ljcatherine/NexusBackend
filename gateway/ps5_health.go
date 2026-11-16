package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ps5HealthTimeout bounds each PS5 probe; a slower PS5 counts as offline.
const ps5HealthTimeout = 3 * time.Second

// ps5ReasonStates are control-plane states reported to callers as the reason the PS5 cannot serve.
var ps5ReasonStates = map[string]bool{
	"restart_failed": true, "switching": true, "loading": true, "no_model": true, "failed": true,
}

// ps5State is the gateway's view of the PS5 used for status and queue decisions.
type ps5State struct {
	Online bool   `json:"online"`
	Engine string `json:"engine,omitempty"`
	Model  string `json:"model,omitempty"`
	Ready  bool   `json:"ready"`
	Busy   bool   `json:"busy"`
	Reason string `json:"reason,omitempty"`
}

// ps5Health polls the PS5 and guards it with a circuit breaker. Only the poller goroutine writes breaker state.
type ps5Health struct {
	controlBase string
	mediaBase   string
	token       func() string // bearer, read per request and never logged
	client      *http.Client
	timeout     time.Duration
	threshold   int           // consecutive failures before the breaker opens
	cooldown    time.Duration // open time before one half-open probe
	now         func() time.Time

	mu       sync.Mutex
	failures int
	open     bool
	probing  bool
	openedAt time.Time
	snapshot ps5State
}

func newPS5Health(controlBase, mediaBase string, token func() string) *ps5Health {
	return &ps5Health{
		controlBase: strings.TrimRight(controlBase, "/"),
		mediaBase:   strings.TrimRight(mediaBase, "/"),
		token:       token,
		client: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
		timeout:   ps5HealthTimeout,
		threshold: 3,
		cooldown:  30 * time.Second,
		now:       time.Now,
		snapshot:  ps5State{Reason: "ps5_offline"},
	}
}

// Run polls every interval until ctx is done.
func (h *ps5Health) Run(ctx context.Context, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		h.Poll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Snapshot returns the state from the most recent poll.
func (h *ps5Health) Snapshot() ps5State {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.snapshot
}

// Poll runs one probe through the breaker, stores and returns the new state.
func (h *ps5Health) Poll(ctx context.Context) ps5State {
	state := ps5State{Reason: "ps5_offline"}
	if h.allow() {
		probed, err := h.probe(ctx)
		h.record(err == nil)
		if err == nil {
			state = probed
		}
	}
	h.mu.Lock()
	h.snapshot = state
	h.mu.Unlock()
	return state
}

// allow reports whether a probe may run. While open, exactly one half-open probe runs after the cooldown.
func (h *ps5Health) allow() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.open {
		return true
	}
	if h.probing || h.now().Sub(h.openedAt) < h.cooldown {
		return false
	}
	h.probing = true
	return true
}

func (h *ps5Health) record(ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.probing = false
	if ok {
		h.failures, h.open = 0, false
		return
	}
	h.failures++
	if h.open || h.failures >= h.threshold {
		h.open, h.openedAt = true, h.now()
	}
}

func (h *ps5Health) probe(ctx context.Context) (ps5State, error) {
	code, body, err := h.get(ctx, h.controlBase+"/v1/nexus/status")
	if err != nil {
		return ps5State{}, err
	}
	var status struct {
		State  string `json:"state"`
		Model  string `json:"model"`
		Engine string `json:"engine"`
		Ready  bool   `json:"ready"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &status) != nil || status.State == "" {
		return ps5State{}, errors.New("ps5 control status invalid")
	}
	state := ps5State{Online: true, Engine: status.Engine, Model: status.Model, Ready: status.Ready,
		Busy: status.State == "generating"}
	if ps5ReasonStates[status.State] {
		state.Reason = status.State
	}
	if status.Engine != "media" {
		return state, nil
	}
	code, body, err = h.get(ctx, h.mediaBase+"/health")
	if err != nil {
		return ps5State{}, err
	}
	var media struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(body, &media)
	if code != http.StatusOK && code != http.StatusServiceUnavailable {
		return ps5State{}, errors.New("ps5 media health invalid")
	}
	state.Ready = code == http.StatusOK && media.Status == "ok"
	if !state.Ready && state.Reason == "" {
		state.Reason = "media_unavailable"
	}
	return state, nil
}

// get returns fixed error strings so the URL, bearer and response body never reach logs or callers.
func (h *ps5Health) get(ctx context.Context, target string) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return 0, nil, errors.New("ps5 health request invalid")
	}
	req.Header.Set("Authorization", "Bearer "+h.token())
	resp, err := h.client.Do(req)
	if err != nil {
		return 0, nil, errors.New("ps5 health unreachable")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return 0, nil, errors.New("ps5 health read failed")
	}
	return resp.StatusCode, body, nil
}
