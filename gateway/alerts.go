package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	alertKindAuthFailures  = "auth_failures"
	alertKindQuotaHit      = "quota_hit"
	alertKindPS5Offline    = "ps5_offline"
	alertKindPS5Online     = "ps5_online"
	alertKindRequestSpike  = "request_spike"
	alertKindRestartFailed = "restart_failed"

	alertWebhookTimeout     = 5 * time.Second
	alertWebhookMaxInFlight = 4
	alertDefaultDedupWindow = 5 * time.Minute
)

// Fixed messages only: the webhook payload never carries request data.
var alertMessages = map[string]string{
	alertKindAuthFailures:  "authentication failures above threshold",
	alertKindQuotaHit:      "usage quota reached",
	alertKindPS5Offline:    "PS5 is offline",
	alertKindPS5Online:     "PS5 is back online",
	alertKindRequestSpike:  "request rate above threshold",
	alertKindRestartFailed: "PS5 restart failed",
}

type alertConfig struct {
	WebhookURL            string // empty disables the webhook; otherwise must be a clean https URL
	AuthFailuresPerMinute int    // alert when failures in one UTC minute exceed this
	RequestsPerMinute     int    // alert when requests in one UTC minute exceed this
	DedupWindow           time.Duration
	Clock                 func() time.Time
	Transport             http.RoundTripper // optional; tests inject a TLS-trusting transport
}

type alertMinuteCounter struct {
	minute time.Time
	n      int
}

func (c *alertMinuteCounter) add(now time.Time) int {
	if m := now.Truncate(time.Minute); !m.Equal(c.minute) {
		c.minute, c.n = m, 0
	}
	c.n++
	return c.n
}

type alertPayload struct {
	Kind      string `json:"kind"`
	Count     int    `json:"count"`
	Timestamp string `json:"timestamp"`
	Message   string `json:"message"`
}

type alertNotifier struct {
	mu           sync.Mutex
	clock        func() time.Time
	authLimit    int
	requestLimit int
	dedup        time.Duration
	auth         alertMinuteCounter
	requests     alertMinuteCounter
	lastFired    map[string]time.Time
	webhook      string
	client       *http.Client
	slots        chan struct{}
	wg           sync.WaitGroup
}

func newAlertNotifier(cfg alertConfig) (*alertNotifier, error) {
	if cfg.AuthFailuresPerMinute <= 0 || cfg.RequestsPerMinute <= 0 {
		return nil, errors.New("alert thresholds must be positive")
	}
	a := &alertNotifier{
		clock:        cfg.Clock,
		authLimit:    cfg.AuthFailuresPerMinute,
		requestLimit: cfg.RequestsPerMinute,
		dedup:        cfg.DedupWindow,
		lastFired:    map[string]time.Time{},
		slots:        make(chan struct{}, alertWebhookMaxInFlight),
	}
	if a.clock == nil {
		a.clock = time.Now
	}
	if a.dedup <= 0 {
		a.dedup = alertDefaultDedupWindow
	}
	if raw := strings.TrimSpace(cfg.WebhookURL); raw != "" {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			// Fixed error text: the URL may embed a secret, so it is never echoed.
			return nil, errors.New("NEXUS_ALERT_WEBHOOK_URL must be a clean HTTPS URL")
		}
		a.webhook = u.String()
		a.client = &http.Client{
			Timeout:   alertWebhookTimeout,
			Transport: cfg.Transport,
			// Never follow redirects: the 3xx is returned and nothing else is contacted.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	return a, nil
}

// recordAuthFailure counts one failed authentication. It takes no subject, IP or token.
func (a *alertNotifier) recordAuthFailure() {
	if a == nil {
		return
	}
	a.mu.Lock()
	n := a.auth.add(a.clock())
	a.mu.Unlock()
	if n > a.authLimit {
		a.fire(alertKindAuthFailures, n)
	}
}

func (a *alertNotifier) recordRequest() {
	if a == nil {
		return
	}
	a.mu.Lock()
	n := a.requests.add(a.clock())
	a.mu.Unlock()
	if n > a.requestLimit {
		a.fire(alertKindRequestSpike, n)
	}
}

func (a *alertNotifier) quotaHit()      { a.fire(alertKindQuotaHit, 1) }
func (a *alertNotifier) ps5Offline()    { a.fire(alertKindPS5Offline, 1) }
func (a *alertNotifier) ps5Online()     { a.fire(alertKindPS5Online, 1) }
func (a *alertNotifier) restartFailed() { a.fire(alertKindRestartFailed, 1) }

// fire logs the alert and, when configured, posts it to the webhook. A repeat of the
// same kind inside the dedup window is dropped. The webhook is fire-and-forget: at
// most alertWebhookMaxInFlight posts run at once, and an alert that finds no free
// slot is logged without a webhook post.
func (a *alertNotifier) fire(kind string, count int) {
	if a == nil {
		return
	}
	now := a.clock()
	a.mu.Lock()
	if last, ok := a.lastFired[kind]; ok && now.Sub(last) < a.dedup {
		a.mu.Unlock()
		return
	}
	a.lastFired[kind] = now
	a.mu.Unlock()

	log.Printf("alert_event kind=%s count=%d", kind, count)
	if a.webhook == "" {
		return
	}
	select {
	case a.slots <- struct{}{}:
	default:
		log.Printf("alert_event kind=%s webhook=dropped", kind)
		return
	}
	body, _ := json.Marshal(alertPayload{ // cannot fail for this struct
		Kind:      kind,
		Count:     count,
		Timestamp: now.UTC().Format(time.RFC3339),
		Message:   alertMessages[kind],
	})
	a.wg.Add(1)
	go a.post(kind, body)
}

func (a *alertNotifier) post(kind string, body []byte) {
	defer a.wg.Done()
	defer func() { <-a.slots }()
	req, err := http.NewRequest(http.MethodPost, a.webhook, bytes.NewReader(body))
	if err != nil {
		log.Printf("alert_event kind=%s webhook=failed", kind)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		// Transport errors can include the URL, so only the outcome is logged.
		log.Printf("alert_event kind=%s webhook=failed", kind)
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	log.Printf("alert_event kind=%s webhook_status=%d", kind, resp.StatusCode)
}

// wait blocks until in-flight webhook posts finish. Used by tests.
func (a *alertNotifier) wait() {
	a.wg.Wait()
}
