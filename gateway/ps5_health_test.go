package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

const ps5TestToken = "test-token-0123456789abcdef0123456789abcdef"

func ps5TestHealth(t *testing.T, control, media http.HandlerFunc) (*ps5Health, *httptest.Server) {
	t.Helper()
	controlServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+ps5TestToken {
			t.Errorf("control request missing bearer")
		}
		control(w, r)
	}))
	t.Cleanup(controlServer.Close)
	mediaURL := ""
	if media != nil {
		mediaServer := httptest.NewServer(media)
		t.Cleanup(mediaServer.Close)
		mediaURL = mediaServer.URL
	}
	return newPS5Health(controlServer.URL, mediaURL, func() string { return ps5TestToken }), controlServer
}

func ps5TestJSON(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
}

func TestPS5HealthMapsControlStates(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		online bool
		ready  bool
		busy   bool
		reason string
	}{
		{"ready", `{"state":"ready","model":"Qwen","engine":"llm","ready":true}`, true, true, false, ""},
		{"generating", `{"state":"generating","model":"Qwen","engine":"llm","ready":true}`, true, true, true, ""},
		{"restart_failed", `{"state":"restart_failed","engine":"llm","ready":false}`, true, false, false, "restart_failed"},
		{"switching", `{"state":"switching","engine":"llm","ready":false}`, true, false, false, "switching"},
		{"loading", `{"state":"loading","engine":"llm","ready":false}`, true, false, false, "loading"},
		{"no_model", `{"state":"no_model","engine":"llm","ready":false}`, true, false, false, "no_model"},
		{"failed", `{"state":"failed","engine":"llm","ready":false}`, true, false, false, "failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := ps5TestHealth(t, ps5TestJSON(tc.body), nil)
			got := h.Poll(context.Background())
			if got.Online != tc.online || got.Ready != tc.ready || got.Busy != tc.busy || got.Reason != tc.reason {
				t.Fatalf("state = %+v, want online=%v ready=%v busy=%v reason=%q", got, tc.online, tc.ready, tc.busy, tc.reason)
			}
			if h.Snapshot() != got {
				t.Fatalf("snapshot %+v does not match poll result %+v", h.Snapshot(), got)
			}
		})
	}
}

func TestPS5HealthBreakerOpensProbesAndCloses(t *testing.T) {
	var fail atomic.Bool
	var hits atomic.Int32
	fail.Store(true)
	h, _ := ps5TestHealth(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		ps5TestJSON(`{"state":"ready","engine":"llm","ready":true}`)(w, r)
	}, nil)
	clock := time.Unix(1_000_000, 0)
	h.now = func() time.Time { return clock }
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if got := h.Poll(ctx); got.Online || got.Reason != "ps5_offline" {
			t.Fatalf("poll %d = %+v, want offline", i, got)
		}
	}
	if hits.Load() != 3 {
		t.Fatalf("hits = %d, want 3", hits.Load())
	}

	// Open: no probe is sent until the cooldown ends.
	h.Poll(ctx)
	clock = clock.Add(29 * time.Second)
	h.Poll(ctx)
	if hits.Load() != 3 {
		t.Fatalf("open breaker sent a probe: hits = %d", hits.Load())
	}

	// Half-open probe at 30 s fails, so the breaker reopens with a fresh cooldown.
	clock = clock.Add(time.Second)
	h.Poll(ctx)
	if hits.Load() != 4 {
		t.Fatalf("half-open probe not sent: hits = %d", hits.Load())
	}
	h.Poll(ctx)
	if hits.Load() != 4 {
		t.Fatalf("reopened breaker sent a probe: hits = %d", hits.Load())
	}

	// Next half-open probe succeeds and closes the breaker.
	fail.Store(false)
	clock = clock.Add(30 * time.Second)
	if got := h.Poll(ctx); !got.Online || !got.Ready {
		t.Fatalf("probe after recovery = %+v, want online and ready", got)
	}
	h.Poll(ctx)
	if hits.Load() != 6 {
		t.Fatalf("closed breaker should probe every poll: hits = %d", hits.Load())
	}
}

func TestPS5HealthHalfOpenAllowsOneProbe(t *testing.T) {
	h := newPS5Health("http://127.0.0.1:1", "", func() string { return ps5TestToken })
	clock := time.Unix(1_000_000, 0)
	h.now = func() time.Time { return clock }
	h.open, h.openedAt = true, clock
	clock = clock.Add(h.cooldown)
	if !h.allow() {
		t.Fatal("first half-open probe was refused")
	}
	if h.allow() {
		t.Fatal("second probe admitted while the half-open probe is in flight")
	}
	h.record(true)
	if !h.allow() {
		t.Fatal("closed breaker refused a probe")
	}
}

func TestPS5HealthMediaEngineChecksMediaHealth(t *testing.T) {
	var mediaStatus atomic.Int32
	mediaStatus.Store(http.StatusOK)
	media := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" || r.Header.Get("Authorization") != "Bearer "+ps5TestToken {
			t.Errorf("unexpected media request %s", r.URL.Path)
		}
		if mediaStatus.Load() == http.StatusOK {
			ps5TestJSON(`{"status":"ok"}`)(w, r)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"status":"unavailable"}`))
	}
	h, _ := ps5TestHealth(t, ps5TestJSON(`{"state":"ready","engine":"media","ready":false}`), media)
	if got := h.Poll(context.Background()); !got.Online || !got.Ready || got.Engine != "media" || got.Reason != "" {
		t.Fatalf("media ok = %+v", got)
	}
	mediaStatus.Store(http.StatusServiceUnavailable)
	if got := h.Poll(context.Background()); !got.Online || got.Ready || got.Reason != "media_unavailable" {
		t.Fatalf("media 503 = %+v, want online, not ready, media_unavailable", got)
	}
}

func TestPS5HealthRejectsBadStatusAndSlowProbes(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"malformed": ps5TestJSON(`not json`),
		"no state":  ps5TestJSON(`{"engine":"llm"}`),
		"slow": func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(time.Second):
			}
		},
	}
	for name, handler := range cases {
		t.Run(name, func(t *testing.T) {
			h, _ := ps5TestHealth(t, handler, nil)
			h.timeout = 20 * time.Millisecond
			if got := h.Poll(context.Background()); got.Online || got.Reason != "ps5_offline" {
				t.Fatalf("state = %+v, want offline", got)
			}
		})
	}
}
