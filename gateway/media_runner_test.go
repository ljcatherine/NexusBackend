package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// mediaRunnerBody holds a marker that must never reach the usage log.
const mediaRunnerBody = `{"prompt":"private prompt marker"}`

// mediaRunnerGateway wires a gateway to the PS5 control and media URLs given, with a usage log in a temp directory.
func mediaRunnerGateway(t *testing.T, controlURL, mediaURL, inferenceURL string, imageLimit int) *Gateway {
	t.Helper()
	credentials := t.TempDir()
	if err := os.WriteFile(filepath.Join(credentials, "ps5-token"), []byte(mockPS5Token), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CREDENTIALS_DIRECTORY", credentials)
	upstream, _ := url.Parse(inferenceURL)
	upstream.Path = "/v1"
	g := NewGateway(Config{
		Upstream:                  upstream,
		UpstreamBearerToken:       mockPS5Token,
		RequestTimeout:            5 * time.Second,
		RequestsPerMinute:         100,
		MetadataRequestsPerMinute: 100,
		MaximumConcurrentAI:       1,
		PS5ControlURL:             controlURL,
		PS5MediaURL:               mediaURL,
		PS5TokenCredential:        "ps5-token",
		PS5PollInterval:           time.Second,
		Quotas:                    quotaPolicy{quotaPermissionImage: {Unit: quotaUnitRequests, Limit: imageLimit}},
	}, nil)
	usage, err := newUsageLog(t.TempDir(), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	g.usage = usage
	return g
}

func mediaRunnerSpec() mediaJobSpec {
	return mediaJobSpec{Route: "/v1/images/generations", Permission: quotaPermissionImage, QuotaAmount: 1,
		Model: "sd-turbo-fp16", InBytes: int64(len(mediaRunnerBody))}
}

func mediaRunnerRequest(ctx context.Context, subject string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(mediaRunnerBody))
	return r.WithContext(context.WithValue(ctx, principalKey{}, subject))
}

// mediaRunnerImageRun is the run function the handlers use: one image call through the provider.
func mediaRunnerImageRun(g *Gateway) mediaRunFunc {
	return func(ctx context.Context, job *queueJob) error {
		res, err := g.ps5.Media(ctx, "image", ProviderRequest(mediaRunnerBody))
		if err != nil {
			return err
		}
		mediaSetResult(ctx, res)
		return nil
	}
}

func mediaRunnerRunningState(g *Gateway) queueState {
	g.queue.mu.Lock()
	defer g.queue.mu.Unlock()
	if g.queue.running == nil {
		return ""
	}
	return g.queue.running.state
}

func mediaRunnerWaitQueued(t *testing.T, g *Gateway, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		g.queue.mu.Lock()
		n := len(g.queue.queued)
		g.queue.mu.Unlock()
		if n == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("queued jobs = %d, want %d", n, want)
		}
		time.Sleep(time.Millisecond)
	}
}

// mediaRunnerUsage returns every usage line written so far.
func mediaRunnerUsage(t *testing.T, g *Gateway) string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(g.usage.dir, "usage-*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(raw)
	}
	return b.String()
}

// mediaRunnerRemaining returns the subject's remaining daily quota for permission, read from the meter.
func mediaRunnerRemaining(g *Gateway, subject, permission string, limit int) int {
	g.quota.mu.Lock()
	defer g.quota.mu.Unlock()
	return limit - g.quota.used[subject][permission]
}

func mediaRunnerSetTimers(t *testing.T, poll, switchTimeout time.Duration) {
	t.Helper()
	oldPoll, oldSwitch := mediaPollInterval, mediaSwitchTimeout
	mediaPollInterval, mediaSwitchTimeout = poll, switchTimeout
	t.Cleanup(func() { mediaPollInterval, mediaSwitchTimeout = oldPoll, oldSwitch })
}

func mediaRunnerSetGrace(t *testing.T, grace time.Duration) {
	t.Helper()
	old := mediaOfflineGrace
	mediaOfflineGrace = grace
	t.Cleanup(func() { mediaOfflineGrace = old })
}

// mediaRunnerControlDropsOff serves the mock's control API. Once the engine switch is accepted, the PS5 goes offline.
// It comes back online at the returnAfter-th status probe made while offline; zero means it stays offline. The
// returned counter counts those offline probes.
func mediaRunnerControlDropsOff(t *testing.T, mock *mockPS5, returnAfter int32) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	routes := mock.controlRoutes()
	var offlineProbes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/nexus/engine/") {
			routes.ServeHTTP(w, r)
			mock.Set(func(s *mockPS5Switches) { s.Offline = true })
			return
		}
		if sw, _ := mock.snapshot(); sw.Offline && r.URL.Path == "/v1/nexus/status" {
			if n := offlineProbes.Add(1); returnAfter > 0 && n == returnAfter {
				mock.Set(func(s *mockPS5Switches) { s.Offline = false })
			}
		}
		routes.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &offlineProbes
}

func TestMediaRunnerSwitchesEngineAndWaitsUntilReady(t *testing.T) {
	mediaRunnerSetTimers(t, 2*time.Millisecond, mediaSwitchTimeout)
	mock := newMockPS5(t)
	inference := httptest.NewServer(mock.inferenceRoutes())
	t.Cleanup(inference.Close)

	// The first two media health probes report unavailable, so the runner must keep waiting after the switch.
	var (
		gw          atomic.Pointer[Gateway]
		healthCalls atomic.Int32
		sawLoading  atomic.Bool
	)
	routes := mock.mediaRoutes()
	media := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" && healthCalls.Add(1) <= 2 {
			if g := gw.Load(); g != nil && mediaRunnerRunningState(g) == queueStateLoading {
				sawLoading.Store(true)
			}
			mockPS5Send(w, http.StatusServiceUnavailable, "media_health_unavailable.json")
			return
		}
		routes.ServeHTTP(w, r)
	}))
	t.Cleanup(media.Close)

	g := mediaRunnerGateway(t, mock.Control.URL, media.URL, inference.URL, 10)
	gw.Store(g)
	if s := g.ps5.health.Poll(context.Background()); !s.Online || s.Engine != "llm" {
		t.Fatalf("PS5 not online in chat mode: %+v", s)
	}

	ran := 0
	run := func(ctx context.Context, job *queueJob) error {
		ran++
		if s := g.ps5Snapshot(); s.Engine != mediaEngine || !s.Ready || healthCalls.Load() < 3 {
			return errors.New("media job ran before the PS5 was ready")
		}
		return mediaRunnerImageRun(g)(ctx, job)
	}
	rec := httptest.NewRecorder()
	if !g.runMediaJob(rec, mediaRunnerRequest(context.Background(), "alice"), mediaRunnerSpec(), run) {
		t.Fatalf("media job failed: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if ran != 1 {
		t.Fatalf("run calls = %d, want 1", ran)
	}
	if !sawLoading.Load() {
		t.Fatal("job did not read as loading while the PS5 was switching")
	}
	if _, engine := mock.snapshot(); engine != mediaEngine {
		t.Fatalf("mock engine = %q, want media", engine)
	}
}

func TestMediaRunnerOfflineReturns503(t *testing.T) {
	mock := newMockPS5(t)
	inference := httptest.NewServer(mock.inferenceRoutes())
	t.Cleanup(inference.Close)
	mock.Set(func(s *mockPS5Switches) { s.Offline = true })

	g := mediaRunnerGateway(t, mock.Control.URL, mock.Media.URL, inference.URL, 10)
	if s := g.ps5.health.Poll(context.Background()); s.Online {
		t.Fatalf("PS5 reported online while offline: %+v", s)
	}
	ran := false
	rec := httptest.NewRecorder()
	ok := g.runMediaJob(rec, mediaRunnerRequest(context.Background(), "alice"), mediaRunnerSpec(),
		func(context.Context, *queueJob) error { ran = true; return nil })
	if ok || ran {
		t.Fatalf("offline media job ran: ok=%v ran=%v", ok, ran)
	}
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "30" ||
		strings.TrimSpace(rec.Body.String()) != `{"error":{"message":"PS5 is offline"}}` {
		t.Fatalf("offline: status=%d retry-after=%q body=%q", rec.Code, rec.Header().Get("Retry-After"), rec.Body.String())
	}
}

func TestMediaRunnerQuotaExceededReturns429AndAlerts(t *testing.T) {
	mock := newMockPS5(t)
	inference := httptest.NewServer(mock.inferenceRoutes())
	t.Cleanup(inference.Close)
	g := mediaRunnerGateway(t, mock.Control.URL, mock.Media.URL, inference.URL, 1)
	g.ps5.health.Poll(context.Background())

	run := mediaRunnerImageRun(g)
	if !g.runMediaJob(httptest.NewRecorder(), mediaRunnerRequest(context.Background(), "alice"), mediaRunnerSpec(), run) {
		t.Fatal("first image within quota failed")
	}
	rec := httptest.NewRecorder()
	if g.runMediaJob(rec, mediaRunnerRequest(context.Background(), "alice"), mediaRunnerSpec(), run) {
		t.Fatal("second image succeeded past the daily quota")
	}
	if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), "quota") {
		t.Fatalf("quota: status=%d body=%q", rec.Code, rec.Body.String())
	}
	g.alerts.mu.Lock()
	_, fired := g.alerts.lastFired[alertKindQuotaHit]
	g.alerts.mu.Unlock()
	if !fired {
		t.Fatal("quota exhaustion raised no alert")
	}
}

func TestMediaRunnerQueueFullReturns429(t *testing.T) {
	mock := newMockPS5(t)
	inference := httptest.NewServer(mock.inferenceRoutes())
	t.Cleanup(inference.Close)
	g := mediaRunnerGateway(t, mock.Control.URL, mock.Media.URL, inference.URL, 10)
	g.ps5.health.Poll(context.Background())
	for range queueDefaultPerSubject {
		if _, err := g.queue.Enqueue(context.Background(), "alice", "llm", "m"); err != nil {
			t.Fatal(err)
		}
	}
	rec := httptest.NewRecorder()
	if g.runMediaJob(rec, mediaRunnerRequest(context.Background(), "alice"), mediaRunnerSpec(), mediaRunnerImageRun(g)) {
		t.Fatal("job ran with a full queue")
	}
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "10" ||
		!strings.Contains(rec.Body.String(), "PS5 queue is full") {
		t.Fatalf("queue full: status=%d retry-after=%q body=%q", rec.Code, rec.Header().Get("Retry-After"), rec.Body.String())
	}
	if got := mediaRunnerRemaining(g, "alice", quotaPermissionImage, 10); got != 10 {
		t.Fatalf("remaining quota after queue-full = %d, want the starting 10", got)
	}
}

func TestMediaRunnerCancelWhileQueuedNeverRuns(t *testing.T) {
	mock := newMockPS5(t)
	inference := httptest.NewServer(mock.inferenceRoutes())
	t.Cleanup(inference.Close)
	g := mediaRunnerGateway(t, mock.Control.URL, mock.Media.URL, inference.URL, 10)
	g.ps5.health.Poll(context.Background())

	// Another subject holds the only PS5 slot, so the media job waits in the queue.
	blocker, err := g.queue.Enqueue(context.Background(), "bob", "llm", "m")
	if err != nil {
		t.Fatal(err)
	}
	if err := g.queue.Acquire(context.Background(), blocker); err != nil {
		t.Fatal(err)
	}
	defer g.queue.Finish(blocker, queueStateDone)

	ctx, cancel := context.WithCancel(context.Background())
	ran := false
	done := make(chan bool, 1)
	go func() {
		rec := httptest.NewRecorder()
		done <- g.runMediaJob(rec, mediaRunnerRequest(ctx, "alice"), mediaRunnerSpec(),
			func(context.Context, *queueJob) error { ran = true; return nil })
	}()
	mediaRunnerWaitQueued(t, g, 1)
	cancel()
	if <-done {
		t.Fatal("cancelled job reported success")
	}
	if ran {
		t.Fatal("cancelled queued job ran")
	}
	if strings.Contains(mediaRunnerUsage(t, g), `"engine":"media"`) {
		t.Fatal("cancelled queued job wrote a usage line")
	}
	if got := mediaRunnerRemaining(g, "alice", quotaPermissionImage, 10); got != 10 {
		t.Fatalf("remaining quota after cancel-while-queued = %d, want the starting 10", got)
	}
}

func TestMediaRunnerCancelWhileRunningRecordsCancelled(t *testing.T) {
	mock := newMockPS5(t)
	inference := httptest.NewServer(mock.inferenceRoutes())
	t.Cleanup(inference.Close)
	g := mediaRunnerGateway(t, mock.Control.URL, mock.Media.URL, inference.URL, 10)
	g.ps5.health.Poll(context.Background())

	started := make(chan struct{})
	run := func(ctx context.Context, job *queueJob) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan bool, 1)
	rec := httptest.NewRecorder()
	go func() { done <- g.runMediaJob(rec, mediaRunnerRequest(ctx, "alice"), mediaRunnerSpec(), run) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("run never started")
	}
	cancel()
	if <-done {
		t.Fatal("cancelled running job reported success")
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("client gone but a body was written: %q", rec.Body.String())
	}
	if usage := mediaRunnerUsage(t, g); !strings.Contains(usage, `"outcome":"cancelled"`) {
		t.Fatalf("usage does not record cancellation: %s", usage)
	}
}

func TestMediaRunnerUsageLineHasNoPrompt(t *testing.T) {
	mock := newMockPS5(t)
	inference := httptest.NewServer(mock.inferenceRoutes())
	t.Cleanup(inference.Close)
	g := mediaRunnerGateway(t, mock.Control.URL, mock.Media.URL, inference.URL, 10)
	g.ps5.health.Poll(context.Background())

	if !g.runMediaJob(httptest.NewRecorder(), mediaRunnerRequest(context.Background(), "alice"), mediaRunnerSpec(), mediaRunnerImageRun(g)) {
		t.Fatal("image job failed")
	}
	raw := mediaRunnerUsage(t, g)
	if strings.Contains(raw, "private prompt marker") {
		t.Fatalf("usage log contains prompt text: %s", raw)
	}
	var rec usageRecord
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &rec); err != nil {
		t.Fatalf("usage line %q: %v", raw, err)
	}
	if rec.Route != "/v1/images/generations" || rec.Engine != mediaEngine || rec.Model != "sd-turbo-fp16" ||
		rec.Outcome != "done" || rec.InBytes != int64(len(mediaRunnerBody)) || rec.OutBytes == 0 {
		t.Fatalf("usage record = %+v", rec)
	}
	if rec.SubjectHash == "alice" || rec.SubjectHash == "" {
		t.Fatalf("subject not hashed: %q", rec.SubjectHash)
	}
}

func TestMediaRunnerSwitchTimeoutFailsWithoutRunning(t *testing.T) {
	mediaRunnerSetTimers(t, 2*time.Millisecond, 50*time.Millisecond)
	mock := newMockPS5(t)
	inference := httptest.NewServer(mock.inferenceRoutes())
	t.Cleanup(inference.Close)
	routes := mock.mediaRoutes()
	media := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			mockPS5Send(w, http.StatusServiceUnavailable, "media_health_unavailable.json")
			return
		}
		routes.ServeHTTP(w, r)
	}))
	t.Cleanup(media.Close)

	g := mediaRunnerGateway(t, mock.Control.URL, media.URL, inference.URL, 10)
	g.ps5.health.Poll(context.Background())
	ran := false
	rec := httptest.NewRecorder()
	if g.runMediaJob(rec, mediaRunnerRequest(context.Background(), "alice"), mediaRunnerSpec(),
		func(context.Context, *queueJob) error { ran = true; return nil }) || ran {
		t.Fatalf("job ran although media never became ready (ran=%v)", ran)
	}
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" ||
		!strings.Contains(rec.Body.String(), "PS5 media is not ready") {
		t.Fatalf("timeout: status=%d body=%q", rec.Code, rec.Body.String())
	}
	if usage := mediaRunnerUsage(t, g); !strings.Contains(usage, `"outcome":"failed"`) {
		t.Fatalf("usage does not record failure: %s", usage)
	}
	if got := mediaRunnerRemaining(g, "alice", quotaPermissionImage, 10); got != 10 {
		t.Fatalf("remaining quota after switch timeout = %d, want the starting 10", got)
	}
}

func TestMediaRunnerAsyncResultReadableOnce(t *testing.T) {
	mock := newMockPS5(t)
	inference := httptest.NewServer(mock.inferenceRoutes())
	t.Cleanup(inference.Close)
	g := mediaRunnerGateway(t, mock.Control.URL, mock.Media.URL, inference.URL, 10)
	g.ps5.health.Poll(context.Background())

	id, err := g.startMediaJob(context.Background(), "alice", mediaRunnerSpec(), mediaRunnerImageRun(g))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var state queueState
	for {
		s, ok := g.queue.Status(id)
		if ok && queueTerminal(s.State) {
			state = s.State
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("async job still %v", s.State)
		}
		time.Sleep(time.Millisecond)
	}
	if state != queueStateDone {
		t.Fatalf("async job state = %s, want done", state)
	}
	if _, ok := mediaResults.Take(id, "bob"); ok {
		t.Fatal("another subject read the result")
	}
	res, ok := mediaResults.Take(id, "alice")
	if !ok || len(res.Body) == 0 || res.ContentType != "application/json" {
		t.Fatalf("owner read: ok=%v type=%q len=%d", ok, res.ContentType, len(res.Body))
	}
	if _, ok := mediaResults.Take(id, "alice"); ok {
		t.Fatal("result readable a second time")
	}
}

func TestMediaRunnerResultStoreTTLAndSizeCap(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	s := newMediaResStore(func() time.Time { return now }, 10*time.Minute, 100)
	put := func(id string, size int) {
		t.Helper()
		if err := s.Put(id, "alice", MediaResult{Body: make([]byte, size)}); err != nil {
			t.Fatalf("put %s: %v", id, err)
		}
	}
	put("a", 40)
	put("b", 40)
	put("c", 40) // 120 bytes would exceed the cap, so the oldest entry (a) goes
	if _, ok := s.Take("a", "alice"); ok {
		t.Fatal("oldest entry survived the size cap")
	}
	if err := s.Put("big", "alice", MediaResult{Body: make([]byte, 101)}); !errors.Is(err, errMediaResTooLarge) {
		t.Fatalf("oversize put: err = %v", err)
	}
	put("f", 10)
	if _, ok := s.Take("f", "mallory"); ok {
		t.Fatal("another subject read the result")
	}
	if _, ok := s.Take("f", "alice"); !ok {
		t.Fatal("owner could not read the result")
	}
	if _, ok := s.Take("b", "alice"); !ok {
		t.Fatal("entry b was evicted early")
	}

	put("e", 5)
	now = now.Add(10*time.Minute - time.Second)
	if _, ok := s.Take("c", "alice"); !ok {
		t.Fatal("entry expired before its TTL")
	}
	put("g", 5)
	now = now.Add(time.Second) // e expires now; g was stored one second later and is still live
	if _, ok := s.Take("e", "alice"); ok {
		t.Fatal("entry readable after its TTL")
	}
	if _, ok := s.Take("g", "alice"); !ok {
		t.Fatal("entry g expired early")
	}
}

func TestMediaRunnerPS5ErrorKeepsQuotaCharge(t *testing.T) {
	mock := newMockPS5(t)
	inference := httptest.NewServer(mock.inferenceRoutes())
	t.Cleanup(inference.Close)
	g := mediaRunnerGateway(t, mock.Control.URL, mock.Media.URL, inference.URL, 10)
	g.ps5.health.Poll(context.Background())

	// An empty prompt makes the PS5 refuse the generation, so the PS5 ran and failed: the quota stays charged.
	run := func(ctx context.Context, job *queueJob) error {
		_, err := g.ps5.Media(ctx, "image", ProviderRequest(`{"prompt":""}`))
		return err
	}
	rec := httptest.NewRecorder()
	if g.runMediaJob(rec, mediaRunnerRequest(context.Background(), "alice"), mediaRunnerSpec(), run) {
		t.Fatal("PS5 error reported success")
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("PS5 error: status=%d body=%q, want 502", rec.Code, rec.Body.String())
	}
	if got := mediaRunnerRemaining(g, "alice", quotaPermissionImage, 10); got != 9 {
		t.Fatalf("remaining quota after PS5 error = %d, want 9 (charge kept)", got)
	}
}

// mediaRunnerWantOffline checks a refusal that the PS5 never ran: 503 "PS5 is offline", Retry-After 30, quota back.
func mediaRunnerWantOffline(t *testing.T, g *Gateway, rec *httptest.ResponseRecorder, ran bool) {
	t.Helper()
	if ran {
		t.Fatal("job ran while the PS5 was offline")
	}
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "30" ||
		strings.TrimSpace(rec.Body.String()) != `{"error":{"message":"PS5 is offline"}}` {
		t.Fatalf("offline: status=%d retry-after=%q body=%q", rec.Code, rec.Header().Get("Retry-After"), rec.Body.String())
	}
	if got := mediaRunnerRemaining(g, "alice", quotaPermissionImage, 10); got != 10 {
		t.Fatalf("remaining quota after offline refusal = %d, want the starting 10", got)
	}
}

func TestMediaRunnerOfflineBeforeSwitchFailsFastAndRefunds(t *testing.T) {
	mock := newMockPS5(t)
	inference := httptest.NewServer(mock.inferenceRoutes())
	t.Cleanup(inference.Close)
	g := mediaRunnerGateway(t, mock.Control.URL, mock.Media.URL, inference.URL, 10)
	g.ps5.health.Poll(context.Background()) // online in chat mode, so admission passes
	// The snapshot is now stale: the PS5 dropped after admission and before the job's switch.
	mock.Set(func(s *mockPS5Switches) { s.Offline = true })

	ran := false
	rec := httptest.NewRecorder()
	start := time.Now()
	ok := g.runMediaJob(rec, mediaRunnerRequest(context.Background(), "alice"), mediaRunnerSpec(),
		func(context.Context, *queueJob) error { ran = true; return nil })
	if ok {
		t.Fatal("offline PS5 reported success")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("offline refusal took %v", elapsed)
	}
	if _, engine := mock.snapshot(); engine != "llm" {
		t.Fatalf("engine switch requested while offline: engine=%q", engine)
	}
	mediaRunnerWantOffline(t, g, rec, ran)
}

func TestMediaRunnerOfflineMidSwitchFailsAfterGrace(t *testing.T) {
	mediaRunnerSetTimers(t, 2*time.Millisecond, 10*time.Second)
	mediaRunnerSetGrace(t, 150*time.Millisecond)
	mock := newMockPS5(t)
	inference := httptest.NewServer(mock.inferenceRoutes())
	t.Cleanup(inference.Close)
	control, _ := mediaRunnerControlDropsOff(t, mock, 0)

	g := mediaRunnerGateway(t, control.URL, mock.Media.URL, inference.URL, 10)
	g.ps5.health.Poll(context.Background())
	ran := false
	rec := httptest.NewRecorder()
	start := time.Now()
	ok := g.runMediaJob(rec, mediaRunnerRequest(context.Background(), "alice"), mediaRunnerSpec(),
		func(context.Context, *queueJob) error { ran = true; return nil })
	if ok {
		t.Fatal("job succeeded although the PS5 stayed offline")
	}
	// The switch timeout is 10 s. Failing well before it shows the offline grace ended the wait.
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("waited %v; the offline grace should end the wait first", elapsed)
	}
	mediaRunnerWantOffline(t, g, rec, ran)
}

func TestMediaRunnerOfflineBriefDuringSwitchThenSucceeds(t *testing.T) {
	mediaRunnerSetTimers(t, 2*time.Millisecond, 10*time.Second)
	mediaRunnerSetGrace(t, 5*time.Second)
	mock := newMockPS5(t)
	inference := httptest.NewServer(mock.inferenceRoutes())
	t.Cleanup(inference.Close)
	control, offlineProbes := mediaRunnerControlDropsOff(t, mock, 3)

	g := mediaRunnerGateway(t, control.URL, mock.Media.URL, inference.URL, 10)
	g.ps5.health.Poll(context.Background())
	rec := httptest.NewRecorder()
	if !g.runMediaJob(rec, mediaRunnerRequest(context.Background(), "alice"), mediaRunnerSpec(), mediaRunnerImageRun(g)) {
		t.Fatalf("job failed after a brief offline window: status=%d body=%q", rec.Code, rec.Body.String())
	}
	if offlineProbes.Load() < 3 {
		t.Fatalf("PS5 was offline for %d probes, want the full window", offlineProbes.Load())
	}
	if _, engine := mock.snapshot(); engine != mediaEngine {
		t.Fatalf("mock engine = %q, want media", engine)
	}
}

func TestMediaRunnerQueuedJobFailsFastWhenPS5DropsOff(t *testing.T) {
	mock := newMockPS5(t)
	inference := httptest.NewServer(mock.inferenceRoutes())
	t.Cleanup(inference.Close)
	g := mediaRunnerGateway(t, mock.Control.URL, mock.Media.URL, inference.URL, 10)
	g.ps5.health.Poll(context.Background())

	// Another subject holds the PS5 slot, so the media job waits behind it.
	blocker, err := g.queue.Enqueue(context.Background(), "bob", "llm", "m")
	if err != nil {
		t.Fatal(err)
	}
	if err := g.queue.Acquire(context.Background(), blocker); err != nil {
		t.Fatal(err)
	}
	ran := false
	rec := httptest.NewRecorder()
	done := make(chan bool, 1)
	go func() {
		done <- g.runMediaJob(rec, mediaRunnerRequest(context.Background(), "alice"), mediaRunnerSpec(),
			func(context.Context, *queueJob) error { ran = true; return nil })
	}()
	mediaRunnerWaitQueued(t, g, 1)

	// The PS5 drops while the job waits, then the blocker finishes and the job reaches the front.
	mock.Set(func(s *mockPS5Switches) { s.Offline = true })
	g.ps5.health.Poll(context.Background())
	g.queue.Finish(blocker, queueStateDone)

	select {
	case ok := <-done:
		if ok {
			t.Fatal("queued job succeeded although the PS5 was offline")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("queued job did not fail fast once it reached the front")
	}
	mediaRunnerWantOffline(t, g, rec, ran)
}
