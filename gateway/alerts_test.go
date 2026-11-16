package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const alertTestSecret = "test-token-0123456789abcdef0123456789abcdef"

func captureAlertLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

func alertTestNotifier(t *testing.T, now *time.Time, webhook string, transport http.RoundTripper) *alertNotifier {
	t.Helper()
	a, err := newAlertNotifier(alertConfig{
		WebhookURL:            webhook,
		AuthFailuresPerMinute: 3,
		RequestsPerMinute:     2,
		DedupWindow:           5 * time.Minute,
		Clock:                 func() time.Time { return *now },
		Transport:             transport,
	})
	if err != nil {
		t.Fatalf("newAlertNotifier: %v", err)
	}
	return a
}

func TestAlertsAuthFailureThresholdAndDedup(t *testing.T) {
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	buf := captureAlertLog(t)
	a := alertTestNotifier(t, &now, "", nil)

	for i := 0; i < 3; i++ {
		a.recordAuthFailure()
	}
	if buf.Len() != 0 {
		t.Fatalf("alert fired at threshold, not above it: %q", buf.String())
	}
	a.recordAuthFailure() // 4th in the minute: above threshold 3
	if !strings.Contains(buf.String(), "alert_event kind=auth_failures count=4") {
		t.Fatalf("missing auth_failures alert: %q", buf.String())
	}

	now = now.Add(30 * time.Second) // same minute, inside dedup window
	for i := 0; i < 3; i++ {
		a.recordAuthFailure()
	}
	if n := strings.Count(buf.String(), "kind=auth_failures"); n != 1 {
		t.Fatalf("dedup failed inside window: %d auth alerts", n)
	}

	now = now.Add(6 * time.Minute) // past the dedup window; counter restarts each minute
	for i := 0; i < 3; i++ {
		a.recordAuthFailure()
	}
	if n := strings.Count(buf.String(), "kind=auth_failures"); n != 1 {
		t.Fatalf("3 failures in a fresh minute must not alert: %d auth alerts", n)
	}
	a.recordAuthFailure()
	if n := strings.Count(buf.String(), "kind=auth_failures"); n != 2 {
		t.Fatalf("expected a second alert after dedup window: %d auth alerts", n)
	}
}

func TestAlertsRequestSpikeUsesOwnThreshold(t *testing.T) {
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	buf := captureAlertLog(t)
	a := alertTestNotifier(t, &now, "", nil) // request limit 2

	a.recordRequest()
	a.recordRequest()
	if buf.Len() != 0 {
		t.Fatalf("spike fired at limit: %q", buf.String())
	}
	a.recordRequest()
	if !strings.Contains(buf.String(), "alert_event kind=request_spike count=3") {
		t.Fatalf("missing request_spike alert: %q", buf.String())
	}
	if strings.Contains(buf.String(), "kind=auth_failures") {
		t.Fatalf("request counter leaked into auth counter: %q", buf.String())
	}
}

func TestAlertsDedupWindowIsPerKind(t *testing.T) {
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	buf := captureAlertLog(t)
	a := alertTestNotifier(t, &now, "", nil)

	a.quotaHit()
	now = now.Add(time.Minute)
	a.quotaHit() // suppressed: same kind inside window
	a.ps5Offline()
	now = now.Add(time.Minute)
	a.ps5Online()
	a.restartFailed()
	now = now.Add(2*time.Minute + 59*time.Second) // 4m59s after the first quota_hit
	a.quotaHit()                                  // still suppressed
	if n := strings.Count(buf.String(), "kind=quota_hit"); n != 1 {
		t.Fatalf("quota_hit dedup: want 1 log line, got %d: %q", n, buf.String())
	}
	now = now.Add(time.Second) // exactly 5m after the first quota_hit
	a.quotaHit()
	if n := strings.Count(buf.String(), "kind=quota_hit"); n != 2 {
		t.Fatalf("quota_hit should fire once window elapsed: got %d", n)
	}
	for _, kind := range []string{"ps5_offline", "ps5_online", "restart_failed"} {
		if !strings.Contains(buf.String(), "kind="+kind) {
			t.Fatalf("missing %s alert: %q", kind, buf.String())
		}
	}
}

func TestAlertsNilNotifierIsNoop(t *testing.T) {
	var a *alertNotifier
	a.recordAuthFailure()
	a.recordRequest()
	a.quotaHit()
	a.ps5Offline()
	a.ps5Online()
	a.restartFailed()
}

func TestAlertsRejectsNonHTTPSWebhook(t *testing.T) {
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	bad := []string{
		"http://example.com/hook",
		"ftp://example.com/hook",
		"file:///etc/passwd",
		"https://user:" + alertTestSecret + "@example.com/hook",
		"https://example.com/hook?token=" + alertTestSecret,
		"https://example.com/hook#" + alertTestSecret,
		"https:///no-host",
		"://bad",
	}
	for _, raw := range bad {
		_, err := newAlertNotifier(alertConfig{WebhookURL: raw, AuthFailuresPerMinute: 1, RequestsPerMinute: 1, Clock: func() time.Time { return now }})
		if err == nil {
			t.Fatalf("webhook %q accepted", raw)
		}
		if strings.Contains(err.Error(), alertTestSecret) || strings.Contains(err.Error(), "example.com") {
			t.Fatalf("webhook error echoes the URL: %v", err)
		}
	}
	if _, err := newAlertNotifier(alertConfig{WebhookURL: "", AuthFailuresPerMinute: 1, RequestsPerMinute: 1}); err != nil {
		t.Fatalf("empty webhook must disable it, not fail: %v", err)
	}
	if _, err := newAlertNotifier(alertConfig{AuthFailuresPerMinute: 0, RequestsPerMinute: 1}); err == nil {
		t.Fatal("zero threshold accepted")
	}
}

func TestAlertsWebhookPayloadHasNoSecrets(t *testing.T) {
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	buf := captureAlertLog(t)
	bodies := make(chan []byte, 4)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		body, _ := io.ReadAll(r.Body)
		bodies <- body
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	a := alertTestNotifier(t, &now, srv.URL+"/hook", srv.Client().Transport)
	// Each recorder takes no request data, so no token, prompt, IP or subject can reach it.
	for i := 0; i < 4; i++ {
		a.recordAuthFailure()
	}
	a.quotaHit()
	a.wait()

	var got []alertPayload
	close(bodies)
	for body := range bodies {
		var p alertPayload
		if err := json.Unmarshal(body, &p); err != nil {
			t.Fatalf("payload not JSON: %v", err)
		}
		var fields map[string]any
		_ = json.Unmarshal(body, &fields)
		if len(fields) != 4 {
			t.Fatalf("payload has unexpected fields: %s", body)
		}
		for _, forbidden := range []string{alertTestSecret, "127.0.0.1", "Bearer", "prompt", "subject"} {
			if strings.Contains(string(body), forbidden) {
				t.Fatalf("payload contains %q: %s", forbidden, body)
			}
		}
		got = append(got, p)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 webhook posts, got %d", len(got))
	}
	// Posts run concurrently, so match by kind rather than arrival order.
	byKind := map[string]alertPayload{}
	for _, p := range got {
		byKind[p.Kind] = p
	}
	auth, okAuth := byKind[alertKindAuthFailures]
	quota, okQuota := byKind[alertKindQuotaHit]
	if !okAuth || auth.Count != 4 || auth.Message != alertMessages[alertKindAuthFailures] {
		t.Fatalf("auth payload wrong: %+v", auth)
	}
	if !okQuota || quota.Count != 1 || quota.Message != alertMessages[alertKindQuotaHit] {
		t.Fatalf("quota payload wrong: %+v", quota)
	}
	if _, err := time.Parse(time.RFC3339, auth.Timestamp); err != nil {
		t.Fatalf("timestamp not RFC3339: %q", auth.Timestamp)
	}
	if strings.Contains(buf.String(), "127.0.0.1") || strings.Contains(buf.String(), alertTestSecret) {
		t.Fatalf("log leaks host or secret: %q", buf.String())
	}
}

func TestAlertsWebhookDoesNotFollowRedirects(t *testing.T) {
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	captureAlertLog(t)
	var elsewhere atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			elsewhere.Add(1)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	defer srv.Close()

	a := alertTestNotifier(t, &now, srv.URL+"/hook", srv.Client().Transport)
	a.ps5Offline()
	a.wait()
	if n := elsewhere.Load(); n != 0 {
		t.Fatalf("redirect was followed %d time(s)", n)
	}
}

func TestAlertsWebhookConcurrencyIsBounded(t *testing.T) {
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	buf := captureAlertLog(t)
	release := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	a := alertTestNotifier(t, &now, srv.URL+"/hook", srv.Client().Transport)
	// Five distinct kinds: the first four take every slot and stay in flight.
	a.quotaHit()
	a.ps5Offline()
	a.ps5Online()
	a.restartFailed()
	a.fire(alertKindRequestSpike, 1)
	close(release)
	a.wait()
	if n := strings.Count(buf.String(), "webhook=dropped"); n != 1 {
		t.Fatalf("want exactly one dropped webhook, got %d: %q", n, buf.String())
	}
}
