package main

// End-to-end resilience tests for the PS5 path (plan U-62). Every request goes through g.Handler(), the real routes,
// so the queue, scheduler, breaker, quota and alerts run as they do in production. The PS5 is the mock in
// mockps5_test.go. Identifiers in this file carry the ps5E2E prefix.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ps5E2EGate holds PS5 chat requests until open is called, so a test can queue work behind a running chat.
type ps5E2EGate struct {
	ch   chan struct{}
	once sync.Once
}

func newPS5E2EGate() *ps5E2EGate { return &ps5E2EGate{ch: make(chan struct{})} }

func (g *ps5E2EGate) open() { g.once.Do(func() { close(g.ch) }) }

// ps5E2EStand is the PS5 inference stand-in as the gateway sees it: chat requests in flight, the peak, and the order
// in which chats were served (by prompt).
type ps5E2EStand struct {
	mu            sync.Mutex
	running, peak int
	order         []string
}

// ps5E2EInference serves the mock's inference routes behind a counter. With a gate, each chat waits on it before the
// mock answers. Cleanup closes the server first, then opens the gate, so a failing test cannot hang on Close.
func ps5E2EInference(t *testing.T, mock *mockPS5, gate *ps5E2EGate) (*httptest.Server, *ps5E2EStand) {
	t.Helper()
	stand := &ps5E2EStand{}
	routes := mock.inferenceRoutes()
	var hold chan struct{}
	if gate != nil {
		hold = gate.ch
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chat := r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions"
		var prompt string
		if chat {
			raw, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(raw))
			var req struct {
				Messages []struct {
					Content string `json:"content"`
				} `json:"messages"`
			}
			if json.Unmarshal(raw, &req) == nil && len(req.Messages) > 0 {
				prompt = req.Messages[0].Content
			}
		}
		stand.mu.Lock()
		stand.running++
		stand.peak = max(stand.peak, stand.running)
		if chat {
			stand.order = append(stand.order, prompt)
		}
		stand.mu.Unlock()
		defer func() {
			stand.mu.Lock()
			stand.running--
			stand.mu.Unlock()
		}()
		if chat && hold != nil {
			<-hold
		}
		routes.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	if gate != nil {
		t.Cleanup(gate.open)
	}
	return server, stand
}

// ps5E2EGateway is testPS5Gateway with every listed subject holding chat, models and the media permissions, and a
// quota large enough that only a test's own quota changes the outcome.
func ps5E2EGateway(t *testing.T, mock *mockPS5, inference *httptest.Server, subjects ...string) *Gateway {
	t.Helper()
	g := testPS5Gateway(t, mock, inference)
	tokens := map[string]string{}
	for _, subject := range subjects {
		g.cfg.SubjectPermissions[subject] = map[string]bool{permissionModels: true, permissionChat: true,
			quotaPermissionImage: true, quotaPermissionVideo: true, quotaPermissionAudio: true}
		tokens[subject+"-token"] = subject
	}
	g.verifier = fakeVerifier{subjects: tokens}
	g.quota = newQuotaMeter(quotaPolicy{
		quotaPermissionImage: {Unit: quotaUnitRequests, Limit: 100},
		quotaPermissionVideo: {Unit: quotaUnitRequests, Limit: 100},
		quotaPermissionAudio: {Unit: quotaUnitRequests, Limit: 100},
	}, time.Now)
	return g
}

// ps5E2EControlCount counts status polls answered and engine switches requested by the mock's control API.
type ps5E2EControlCount struct{ polls, switches atomic.Int32 }

// ps5E2ECountControl replaces the mock's control server with one that counts. newMockPS5's cleanup closes whichever
// control server is current, so the replacement is closed with the rest of the mock. Install it before the gateway.
func ps5E2ECountControl(mock *mockPS5) *ps5E2EControlCount {
	c := &ps5E2EControlCount{}
	routes := mock.controlRoutes()
	counted := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/nexus/engine/") {
			c.switches.Add(1)
		}
		routes.ServeHTTP(w, r)
		if r.Method == http.MethodGet && r.URL.Path == "/v1/nexus/status" {
			c.polls.Add(1) // counted after the answer, so a counted poll has seen the PS5 in its current state
		}
	}))
	mock.Control.Close()
	mock.Control = counted
	return c
}

// ps5E2EOnline polls the PS5 once and fails unless it is online in chat mode.
func ps5E2EOnline(t *testing.T, g *Gateway) {
	t.Helper()
	if s := g.ps5.health.Poll(context.Background()); !s.Online || s.Engine != "llm" {
		t.Fatalf("PS5 not online in chat mode: %+v", s)
	}
}

// ps5E2EQueue reports whether a job is running and how many are waiting.
func ps5E2EQueue(g *Gateway) (bool, int) {
	g.queue.mu.Lock()
	defer g.queue.mu.Unlock()
	return g.queue.running != nil, len(g.queue.queued)
}

func ps5E2EWait(t *testing.T, what string, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !ready() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// ps5E2EGo runs handler in a goroutine and returns the channel that delivers the recorded response.
func ps5E2EGo(handler http.Handler, req *http.Request) <-chan *httptest.ResponseRecorder {
	ch := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		ch <- rec
	}()
	return ch
}

func ps5E2EReceive(t *testing.T, ch <-chan *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	select {
	case rec := <-ch:
		return rec
	case <-time.After(30 * time.Second):
		t.Fatal("request did not finish")
		return nil
	}
}

func ps5E2EPost(token, path string, body any) *http.Request {
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	return req
}

func ps5E2EGet(token, path string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	return req
}

func ps5E2EChat(token, prompt string) *http.Request {
	return ps5E2EPost(token, "/v1/chat/completions", map[string]any{
		"model": "model-installed-by-owner", "messages": []map[string]string{{"role": "user", "content": prompt}}, "stream": false,
	})
}

func ps5E2EImage(token, prompt, model string) *http.Request {
	return ps5E2EPost(token, "/v1/images/generations", map[string]any{
		"prompt": prompt, "model": model, "width": 256, "height": 256,
	})
}

// ps5E2ELog collects log output. log.Logger serialises writes, and the mutex covers reads from the test goroutine.
type ps5E2ELog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *ps5E2ELog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *ps5E2ELog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// ps5E2ECaptureLogs sends the standard logger to a buffer until the test ends.
func ps5E2ECaptureLogs(t *testing.T) *ps5E2ELog {
	t.Helper()
	out := &ps5E2ELog{}
	previous := log.Writer()
	log.SetOutput(out)
	t.Cleanup(func() { log.SetOutput(previous) })
	return out
}

// ps5E2ESubject returns the subject of a prompt written as "<subject>-<n>".
func ps5E2ESubject(prompt string) string {
	subject, _, _ := strings.Cut(prompt, "-")
	return subject
}

// TestPS5E2EFairnessUnderLoad: three subjects each submit three chats while the first is running. The PS5 serves
// them in turn, so no subject is served twice in a row while another is waiting, and only one chat is in flight.
func TestPS5E2EFairnessUnderLoad(t *testing.T) {
	mock := newMockPS5(t)
	gate := newPS5E2EGate()
	inference, stand := ps5E2EInference(t, mock, gate)
	g := ps5E2EGateway(t, mock, inference, "alice", "bob", "carol")
	ps5E2EOnline(t, g)
	handler := g.Handler()

	pending := []<-chan *httptest.ResponseRecorder{ps5E2EGo(handler, ps5E2EChat("alice-token", "alice-1"))}
	ps5E2EWait(t, "first chat running", func() bool { running, _ := ps5E2EQueue(g); return running })
	for _, subject := range []string{"alice", "bob", "carol"} {
		for n := 1; n <= 3; n++ {
			if subject == "alice" && n == 1 {
				continue
			}
			pending = append(pending, ps5E2EGo(handler, ps5E2EChat(subject+"-token", fmt.Sprintf("%s-%d", subject, n))))
		}
	}
	// Release only once all nine chats are in the queue, so the scheduler chooses among them.
	ps5E2EWait(t, "eight chats queued", func() bool { _, queued := ps5E2EQueue(g); return queued == 8 })
	gate.open()
	for i, ch := range pending {
		if rec := ps5E2EReceive(t, ch); rec.Code != http.StatusOK {
			t.Errorf("chat %d: status %d, want 200", i, rec.Code)
		}
	}

	stand.mu.Lock()
	order, peak := append([]string(nil), stand.order...), stand.peak
	stand.mu.Unlock()
	if len(order) != 9 {
		t.Fatalf("PS5 served %d chats, want 9", len(order))
	}
	for i := 1; i < len(order); i++ {
		if ps5E2ESubject(order[i]) == ps5E2ESubject(order[i-1]) {
			t.Fatalf("subject served twice in a row while others waited: order %v", order)
		}
	}
	if peak != 1 {
		t.Fatalf("peak concurrent PS5 chats = %d, want 1", peak)
	}
}

// TestPS5E2EPerSubjectQueueLimit: once one subject has three chats waiting, her next request gets 429 and so does
// her image request (which the per-minute rate budget does not cover). Other subjects still queue, and the refused
// image is refunded.
func TestPS5E2EPerSubjectQueueLimit(t *testing.T) {
	mock := newMockPS5(t)
	gate := newPS5E2EGate()
	inference, _ := ps5E2EInference(t, mock, gate)
	g := ps5E2EGateway(t, mock, inference, "alice", "bob", "carol")
	ps5E2EOnline(t, g)
	handler := g.Handler()

	pending := []<-chan *httptest.ResponseRecorder{ps5E2EGo(handler, ps5E2EChat("alice-token", "alice-1"))}
	ps5E2EWait(t, "first chat running", func() bool { running, _ := ps5E2EQueue(g); return running })
	for n := 2; n <= 4; n++ {
		pending = append(pending, ps5E2EGo(handler, ps5E2EChat("alice-token", fmt.Sprintf("alice-%d", n))))
	}
	ps5E2EWait(t, "three of alice's chats waiting", func() bool { _, queued := ps5E2EQueue(g); return queued == queueDefaultPerSubject })

	refused := httptest.NewRecorder()
	handler.ServeHTTP(refused, ps5E2EChat("alice-token", "alice-5"))
	if refused.Code != http.StatusTooManyRequests || refused.Header().Get("Retry-After") == "" ||
		!strings.Contains(refused.Body.String(), "PS5 queue is full") {
		t.Fatalf("fourth waiting chat: status=%d retry-after=%q body=%q", refused.Code, refused.Header().Get("Retry-After"), refused.Body.String())
	}
	imageRefused := httptest.NewRecorder()
	handler.ServeHTTP(imageRefused, ps5E2EImage("alice-token", "queue limit", "sd-turbo-fp16"))
	if imageRefused.Code != http.StatusTooManyRequests || !strings.Contains(imageRefused.Body.String(), "PS5 queue is full") {
		t.Fatalf("image over the queue limit: status=%d body=%q", imageRefused.Code, imageRefused.Body.String())
	}
	g.quota.mu.Lock()
	imageUsed := g.quota.used["alice"][quotaPermissionImage]
	g.quota.mu.Unlock()
	if imageUsed != 0 {
		t.Fatalf("refused image left %d units of quota used, want 0", imageUsed)
	}

	pending = append(pending, ps5E2EGo(handler, ps5E2EChat("bob-token", "bob-1")), ps5E2EGo(handler, ps5E2EChat("carol-token", "carol-1")))
	ps5E2EWait(t, "bob and carol queued behind alice", func() bool { _, queued := ps5E2EQueue(g); return queued == 5 })
	gate.open()
	for i, ch := range pending {
		if rec := ps5E2EReceive(t, ch); rec.Code != http.StatusOK {
			t.Errorf("queued chat %d: status %d, want 200", i, rec.Code)
		}
	}
}

// TestPS5E2EBreakerOfflineAndRecovery: the mock goes offline, the breaker opens and chat and media answer 503 with
// Retry-After. When the mock returns, the half-open probe closes the breaker. The offline and online alerts are each
// logged once.
func TestPS5E2EBreakerOfflineAndRecovery(t *testing.T) {
	logs := ps5E2ECaptureLogs(t)
	mock := newMockPS5(t)
	control := ps5E2ECountControl(mock)
	inference, _ := ps5E2EInference(t, mock, nil)
	g := ps5E2EGateway(t, mock, inference, "alice")
	g.cfg.PS5PollInterval = 5 * time.Millisecond
	// Zero cooldown: while open, every watcher tick is the half-open probe, so recovery follows the next tick and no
	// wall-clock cooldown can expire in the middle of an assertion.
	g.ps5.health.cooldown = 0
	ps5E2EOnline(t, g) // poll 1

	// The watcher raises the online and offline alerts. Its first poll is the baseline, so the mock must not go
	// offline until that poll has been answered online (poll 2). The breaker cannot open before then, because it needs
	// three failed polls. Its stop runs before the mock closes.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		g.watchPS5(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	ps5E2EWait(t, "watcher baseline poll", func() bool { return control.polls.Load() >= 2 })
	breakerOpen := func() bool {
		g.ps5.health.mu.Lock()
		defer g.ps5.health.mu.Unlock()
		return g.ps5.health.open
	}

	mock.Set(func(s *mockPS5Switches) { s.Offline = true })
	ps5E2EWait(t, "breaker to open on the PS5 going offline", breakerOpen)
	if g.ps5Snapshot().Online {
		t.Fatal("status reports online while the breaker is open")
	}
	handler := g.Handler()
	for _, tc := range []struct {
		name string
		req  *http.Request
	}{
		{"chat", ps5E2EChat("alice-token", "during outage")},
		{"image", ps5E2EImage("alice-token", "during outage", "sd-turbo-fp16")},
	} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, tc.req)
		if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" ||
			strings.TrimSpace(rec.Body.String()) != `{"error":{"message":"PS5 is offline"}}` {
			t.Errorf("%s while offline: status=%d retry-after=%q body=%q", tc.name, rec.Code, rec.Header().Get("Retry-After"), rec.Body.String())
		}
	}
	ps5E2EWait(t, "ps5_offline alert", func() bool { return strings.Contains(logs.String(), "alert_event kind=ps5_offline count=1") })

	mock.Set(func(s *mockPS5Switches) { s.Offline = false })
	ps5E2EWait(t, "half-open probe to close the breaker", func() bool { return g.ps5Snapshot().Online && !breakerOpen() })
	recovered := httptest.NewRecorder()
	handler.ServeHTTP(recovered, ps5E2EChat("alice-token", "after recovery"))
	if recovered.Code != http.StatusOK {
		t.Fatalf("chat after recovery: status %d, want 200", recovered.Code)
	}
	ps5E2EWait(t, "ps5_online alert", func() bool { return strings.Contains(logs.String(), "alert_event kind=ps5_online count=1") })

	out := logs.String()
	if n := strings.Count(out, "alert_event kind=ps5_offline "); n != 1 {
		t.Errorf("ps5_offline logged %d times, want 1", n)
	}
	if n := strings.Count(out, "alert_event kind=ps5_online "); n != 1 {
		t.Errorf("ps5_online logged %d times, want 1", n)
	}
	if strings.Index(out, "kind=ps5_offline") > strings.Index(out, "kind=ps5_online") {
		t.Error("ps5_online logged before ps5_offline")
	}
}

// ps5E2EStartWatcher runs the PS5 watcher until the test ends, and waits for its first poll.
func ps5E2EStartWatcher(t *testing.T, g *Gateway, control *ps5E2EControlCount) {
	t.Helper()
	g.cfg.PS5PollInterval = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		g.watchPS5(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	ps5E2EWait(t, "watcher first poll", func() bool { return control.polls.Load() >= 1 })
}

// TestPS5E2EWatcherOfflineAtStartAlerts: a PS5 that is already offline when the watcher starts raises ps5_offline on
// its first poll, and no ps5_online.
func TestPS5E2EWatcherOfflineAtStartAlerts(t *testing.T) {
	logs := ps5E2ECaptureLogs(t)
	mock := newMockPS5(t)
	control := ps5E2ECountControl(mock)
	mock.Set(func(s *mockPS5Switches) { s.Offline = true })
	inference, _ := ps5E2EInference(t, mock, nil)
	g := ps5E2EGateway(t, mock, inference, "alice")
	ps5E2EStartWatcher(t, g, control)

	ps5E2EWait(t, "ps5_offline alert on the first poll", func() bool {
		return strings.Contains(logs.String(), "alert_event kind=ps5_offline count=1")
	})
	if strings.Contains(logs.String(), "kind=ps5_online") {
		t.Fatal("ps5_online raised for a PS5 that never came up")
	}
}

// TestPS5E2EWatcherHealthyFirstPollIsQuiet: a PS5 that is online when the watcher starts raises no alert.
func TestPS5E2EWatcherHealthyFirstPollIsQuiet(t *testing.T) {
	logs := ps5E2ECaptureLogs(t)
	mock := newMockPS5(t)
	control := ps5E2ECountControl(mock)
	inference, _ := ps5E2EInference(t, mock, nil)
	g := ps5E2EGateway(t, mock, inference, "alice")
	ps5E2EStartWatcher(t, g, control)

	// Wait for several healthy polls after the baseline, so a stray alert would have been raised by now.
	ps5E2EWait(t, "healthy watcher polls", func() bool { return control.polls.Load() >= 4 })
	if out := logs.String(); strings.Contains(out, "alert_event kind=") {
		t.Fatalf("alerts raised for a healthy PS5: %s", out)
	}
}

// TestPS5E2EModeSwitchGrouping: with the PS5 in chat mode, the queue holds chat, image, chat, image from four
// subjects. The scheduler serves the two chats first, then switches to media once. Served in arrival order, the PS5
// would switch three times, and the second chat would be refused while it switched back.
func TestPS5E2EModeSwitchGrouping(t *testing.T) {
	mock := newMockPS5(t)
	control := ps5E2ECountControl(mock)

	gate := newPS5E2EGate()
	inference, _ := ps5E2EInference(t, mock, gate)
	g := ps5E2EGateway(t, mock, inference, "alice", "bob", "carol", "dave")
	ps5E2EOnline(t, g)
	handler := g.Handler()

	pending := []<-chan *httptest.ResponseRecorder{ps5E2EGo(handler, ps5E2EChat("alice-token", "alice-1"))}
	ps5E2EWait(t, "first chat running", func() bool { running, _ := ps5E2EQueue(g); return running })
	pending = append(pending, ps5E2EGo(handler, ps5E2EImage("bob-token", "bob image", "sd-turbo-fp16")))
	ps5E2EWait(t, "bob's image queued", func() bool { _, queued := ps5E2EQueue(g); return queued == 1 })
	pending = append(pending, ps5E2EGo(handler, ps5E2EChat("carol-token", "carol-1")))
	ps5E2EWait(t, "carol's chat queued", func() bool { _, queued := ps5E2EQueue(g); return queued == 2 })
	pending = append(pending, ps5E2EGo(handler, ps5E2EImage("dave-token", "dave image", "sd-turbo-fp16")))
	ps5E2EWait(t, "dave's image queued", func() bool { _, queued := ps5E2EQueue(g); return queued == 3 })
	gate.open()
	for i, ch := range pending {
		if rec := ps5E2EReceive(t, ch); rec.Code != http.StatusOK {
			t.Errorf("request %d: status %d, want 200", i, rec.Code)
		}
	}
	n := control.switches.Load()
	if n < 1 {
		t.Fatal("no engine switch reached the mock; the image never ran on media")
	}
	if n > 2 {
		t.Fatalf("engine switches = %d, want at most 2 (the scheduler should group by engine)", n)
	}
}

// TestPS5E2EVideoPath: POST a video, poll the job to done, then fetch the MP4 once. A second fetch is a 404.
func TestPS5E2EVideoPath(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	mock := newMockPS5(t)
	inference, _ := ps5E2EInference(t, mock, nil)
	g := ps5E2EGateway(t, mock, inference, "alice")
	ps5E2EOnline(t, g)
	handler := g.Handler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, ps5E2EPost("alice-token", "/v1/videos/generations", map[string]any{
		"prompt": "ps5e2e video prompt", "width": 128, "height": 128, "frames": 5, "format": "mp4",
	}))
	var accepted struct {
		ID string `json:"id"`
	}
	if rec.Code != http.StatusAccepted || json.Unmarshal(rec.Body.Bytes(), &accepted) != nil || !aiJobIDPattern.MatchString(accepted.ID) {
		t.Fatalf("video POST: status=%d body=%q, want 202 with a job id", rec.Code, rec.Body.String())
	}

	var state queueState
	ps5E2EWait(t, "video job to finish", func() bool {
		job := httptest.NewRecorder()
		handler.ServeHTTP(job, ps5E2EGet("alice-token", "/v1/ai/jobs/"+accepted.ID))
		var detail aiJobDetail
		if job.Code != http.StatusOK || json.Unmarshal(job.Body.Bytes(), &detail) != nil {
			t.Fatalf("job poll: status %d body %q", job.Code, job.Body.String())
		}
		state = detail.State
		return queueTerminal(state)
	})
	if state != queueStateDone {
		t.Fatalf("video job state = %s, want done", state)
	}

	result := httptest.NewRecorder()
	handler.ServeHTTP(result, ps5E2EGet("alice-token", "/v1/ai/jobs/"+accepted.ID+"/result"))
	if result.Code != http.StatusOK || result.Header().Get("Content-Type") != "video/mp4" {
		t.Fatalf("video result: status=%d content-type=%q", result.Code, result.Header().Get("Content-Type"))
	}
	if body := result.Body.Bytes(); len(body) < 12 || string(body[4:8]) != "ftyp" {
		t.Fatalf("video result is not an MP4 (%d bytes)", len(body))
	}
	again := httptest.NewRecorder()
	handler.ServeHTTP(again, ps5E2EGet("alice-token", "/v1/ai/jobs/"+accepted.ID+"/result"))
	if again.Code != http.StatusNotFound {
		t.Fatalf("second result fetch: status %d, want 404 (results are read once)", again.Code)
	}
}

// TestPS5E2EQuotaHit: a refusal before the PS5 (bad model, PS5 offline) uses no quota. The third image in the day
// gets 429 with a quota message and raises a quota_hit alert.
func TestPS5E2EQuotaHit(t *testing.T) {
	logs := ps5E2ECaptureLogs(t)
	mock := newMockPS5(t)
	inference, _ := ps5E2EInference(t, mock, nil)
	g := ps5E2EGateway(t, mock, inference, "alice")
	g.quota = newQuotaMeter(quotaPolicy{quotaPermissionImage: {Unit: quotaUnitRequests, Limit: 2}}, time.Now)
	ps5E2EOnline(t, g)
	handler := g.Handler()
	send := func(req *http.Request) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	if rec := send(ps5E2EImage("alice-token", "bad model", "not-a-model")); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown model: status %d, want 400", rec.Code)
	}
	mock.Set(func(s *mockPS5Switches) { s.Offline = true })
	g.ps5.health.Poll(context.Background()) // one failed probe is enough to mark the PS5 offline
	if rec := send(ps5E2EImage("alice-token", "offline", "sd-turbo-fp16")); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("image while offline: status %d, want 503", rec.Code)
	}
	mock.Set(func(s *mockPS5Switches) { s.Offline = false })
	ps5E2EOnline(t, g)

	for i := 1; i <= 2; i++ {
		if rec := send(ps5E2EImage("alice-token", "within quota", "sd-turbo-fp16")); rec.Code != http.StatusOK {
			t.Fatalf("image %d within quota: status %d, want 200", i, rec.Code)
		}
	}
	rec := send(ps5E2EImage("alice-token", "over quota", "sd-turbo-fp16"))
	if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), "quota") {
		t.Fatalf("image over quota: status=%d body=%q, want 429 mentioning quota", rec.Code, rec.Body.String())
	}
	if !strings.Contains(logs.String(), "alert_event kind=quota_hit count=1") {
		t.Error("quota_hit alert not logged")
	}
}

// TestPS5E2ENoSecretsInLogs runs chat, image, transcription, a quota refusal, an offline refusal and a bad token with
// log output captured throughout. The log must contain none of the bearer tokens, prompt text or base64 audio.
func TestPS5E2ENoSecretsInLogs(t *testing.T) {
	logs := ps5E2ECaptureLogs(t)
	mock := newMockPS5(t)
	inference, _ := ps5E2EInference(t, mock, nil)
	g := ps5E2EGateway(t, mock, inference, "alice")
	g.quota = newQuotaMeter(quotaPolicy{
		quotaPermissionImage: {Unit: quotaUnitRequests, Limit: 1},
		quotaPermissionAudio: {Unit: quotaUnitRequests, Limit: 100},
	}, time.Now)
	ps5E2EOnline(t, g)
	handler := g.Handler()
	send := func(req *http.Request) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	const (
		chatPrompt  = "ps5e2e-chat-prompt-marker"
		imagePrompt = "ps5e2e-image-prompt-marker"
		badToken    = "ps5e2e-unknown-token-value"
	)
	audio := base64.StdEncoding.EncodeToString(mockPS5WAVBytes())
	secrets := map[string]string{
		"PS5 bearer":       mockPS5Token,
		"client bearer":    "alice-token",
		"unknown bearer":   badToken,
		"chat prompt":      chatPrompt,
		"image prompt":     imagePrompt,
		"base64 audio":     audio,
		"audio prefix b64": audio[:64],
	}

	if rec := send(ps5E2EChat("alice-token", chatPrompt)); rec.Code != http.StatusOK {
		t.Fatalf("chat: status %d", rec.Code)
	}
	if rec := send(ps5E2EImage("alice-token", imagePrompt, "sd-turbo-fp16")); rec.Code != http.StatusOK {
		t.Fatalf("image: status %d", rec.Code)
	}
	if rec := send(ps5E2EImage("alice-token", imagePrompt, "sd-turbo-fp16")); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("image over quota: status %d, want 429", rec.Code)
	}
	if rec := send(ps5E2EPost("alice-token", "/v1/audio/transcriptions", map[string]any{"audio_b64": audio})); rec.Code != http.StatusOK {
		t.Fatalf("transcription: status %d", rec.Code)
	}
	if rec := send(ps5E2EChat(badToken, chatPrompt)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad token: status %d, want 401", rec.Code)
	}
	mock.Set(func(s *mockPS5Switches) { s.Offline = true })
	g.ps5.health.Poll(context.Background())
	if rec := send(ps5E2EChat("alice-token", chatPrompt)); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("chat while offline: status %d, want 503", rec.Code)
	}

	out := logs.String()
	if !strings.Contains(out, "request_id=") {
		t.Fatal("no request lines captured; the scan below would prove nothing")
	}
	for label, secret := range secrets {
		if strings.Contains(out, secret) {
			t.Errorf("log output contains the %s", label)
		}
	}
}
