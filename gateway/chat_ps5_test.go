package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// chatPS5Stand counts requests that reach the PS5 inference stand-in. peak is the most chats in flight at once.
type chatPS5Stand struct {
	mu      sync.Mutex
	running int
	peak    int
	entered chan struct{} // one signal per request that reaches the stand-in
}

func (s *chatPS5Stand) peakInFlight() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.peak
}

// chatPS5Inference serves the mock's inference routes behind a counter. When hold is non-nil, each request waits on
// it before the mock answers, so a test can keep the first chat running while it inspects the queue.
func chatPS5Inference(routes http.Handler, hold <-chan struct{}) (*httptest.Server, *chatPS5Stand) {
	stand := &chatPS5Stand{entered: make(chan struct{}, 16)}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stand.mu.Lock()
		stand.running++
		stand.peak = max(stand.peak, stand.running)
		stand.mu.Unlock()
		defer func() {
			stand.mu.Lock()
			stand.running--
			stand.mu.Unlock()
		}()
		stand.entered <- struct{}{}
		if hold != nil {
			<-hold
		}
		routes.ServeHTTP(w, r)
	}))
	return server, stand
}

func chatPS5StreamRequest(ctx context.Context, token string) *http.Request {
	body := `{"model":"model-installed-by-owner","messages":[{"role":"user","content":"hello"}],"stream":true}`
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer "+token)
	return request
}

// chatPS5WantStream is the exact SSE body the mock writes for a streamed chat, ending with [DONE].
// With errorEvent set, the mock replaces the last chunk with the error event.
func chatPS5WantStream(errorEvent bool) string {
	events := mockPS5Events("inference_chat_stream.json")
	if errorEvent {
		events = events[:len(events)-1]
	}
	var b strings.Builder
	for _, e := range events {
		b.WriteString("data: " + mockPS5Compact(e) + "\n\n")
	}
	if errorEvent {
		b.WriteString("data: " + mockPS5Compact(mockPS5Read("inference_error_event.json")) + "\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

func chatPS5Status(t *testing.T, handler http.Handler, token string) aiStatusResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/ai/status", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	handler.ServeHTTP(rec, request)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d %s", rec.Code, rec.Body.String())
	}
	var status aiStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatalf("status body: %v", err)
	}
	return status
}

func chatPS5Wait(t *testing.T, what string, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ready() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestChatPS5SerialisesStreams(t *testing.T) {
	mock := newMockPS5(t)
	inference, stand := chatPS5Inference(mock.inferenceRoutes(), nil)
	defer inference.Close()
	gateway := testPS5Gateway(t, mock, inference)
	if state := gateway.ps5.health.Poll(context.Background()); !state.Online || state.Engine != "llm" {
		t.Fatalf("PS5 not online for chat: %+v", state)
	}
	// Slow delays each PS5 response, so the two chats overlap unless the queue serialises them.
	mock.Set(func(s *mockPS5Switches) { s.Slow = 100 * time.Millisecond })
	handler := gateway.Handler()

	responses := make([]*httptest.ResponseRecorder, 2)
	var wg sync.WaitGroup
	for i, token := range []string{"alice-token", "bob-token"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			responses[i] = httptest.NewRecorder()
			handler.ServeHTTP(responses[i], chatPS5StreamRequest(context.Background(), token))
		}()
	}
	wg.Wait()

	want := chatPS5WantStream(false)
	for i, rec := range responses {
		if rec.Code != http.StatusOK {
			t.Fatalf("chat %d: status %d, want 200", i, rec.Code)
		}
		if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
			t.Fatalf("chat %d: content type %q, want text/event-stream", i, got)
		}
		if rec.Body.String() != want {
			t.Fatalf("chat %d: SSE body differs from the PS5 stream\ngot:  %q\nwant: %q", i, rec.Body.String(), want)
		}
	}
	if peak := stand.peakInFlight(); peak != 1 {
		t.Fatalf("peak concurrent PS5 chats = %d, want 1", peak)
	}
}

func TestChatPS5QueuePositionInStatus(t *testing.T) {
	mock := newMockPS5(t)
	release := make(chan struct{})
	open := sync.OnceFunc(func() { close(release) })
	inference, stand := chatPS5Inference(mock.inferenceRoutes(), release)
	defer inference.Close()
	defer open() // runs before inference.Close, so a held chat cannot block the close
	gateway := testPS5Gateway(t, mock, inference)
	if state := gateway.ps5.health.Poll(context.Background()); !state.Online {
		t.Fatalf("PS5 not online: %+v", state)
	}
	handler := gateway.Handler()

	// Bob's chat takes the PS5 and is held inside the stand-in.
	bob := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, chatPS5StreamRequest(context.Background(), "bob-token"))
		bob <- rec
	}()
	<-stand.entered

	// Alice's chat waits behind it. Alice has the models permission, so she can read the status.
	alice := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, chatPS5StreamRequest(context.Background(), "alice-token"))
		alice <- rec
	}()
	chatPS5Wait(t, "alice's job to be queued", func() bool {
		s := chatPS5Status(t, handler, "alice-token")
		return len(s.Jobs) == 1 && s.Jobs[0].State == queueStateQueued
	})
	status := chatPS5Status(t, handler, "alice-token")
	// Only alice's own job is listed, so bob's running chat stays hidden from her.
	if !status.Online || status.QueueLength != 1 || len(status.Jobs) != 1 ||
		status.Jobs[0].Position != 1 || status.Jobs[0].State != queueStateQueued {
		t.Fatalf("alice's status while bob runs: %+v, want one queued job at position 1", status)
	}

	open()
	for name, ch := range map[string]chan *httptest.ResponseRecorder{"bob": bob, "alice": alice} {
		rec := <-ch
		if rec.Code != http.StatusOK || rec.Body.String() != chatPS5WantStream(false) {
			t.Fatalf("%s: status %d, body %q", name, rec.Code, rec.Body.String())
		}
	}
}

func TestChatPS5OfflineRetryAfter(t *testing.T) {
	mock := newMockPS5(t)
	mock.Set(func(s *mockPS5Switches) { s.Offline = true })
	inference, stand := chatPS5Inference(mock.inferenceRoutes(), nil)
	defer inference.Close()
	gateway := testPS5Gateway(t, mock, inference)
	if state := gateway.ps5.health.Poll(context.Background()); state.Online {
		t.Fatalf("PS5 reported online while offline: %+v", state)
	}

	rec := httptest.NewRecorder()
	gateway.Handler().ServeHTTP(rec, chatPS5StreamRequest(context.Background(), "alice-token"))
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "30" ||
		strings.TrimSpace(rec.Body.String()) != `{"error":{"message":"PS5 is offline"}}` {
		t.Fatalf("offline chat: status=%d retry-after=%q body=%q", rec.Code, rec.Header().Get("Retry-After"), rec.Body.String())
	}
	if len(stand.entered) != 0 {
		t.Fatal("offline chat reached the PS5 inference stand-in")
	}
}

func TestChatPS5ErrorEventPassedThrough(t *testing.T) {
	mock := newMockPS5(t)
	mock.Set(func(s *mockPS5Switches) { s.ErrorEvent = true })
	inference, _ := chatPS5Inference(mock.inferenceRoutes(), nil)
	defer inference.Close()
	gateway := testPS5Gateway(t, mock, inference)
	if state := gateway.ps5.health.Poll(context.Background()); !state.Online {
		t.Fatalf("PS5 not online: %+v", state)
	}

	rec := httptest.NewRecorder()
	gateway.Handler().ServeHTTP(rec, chatPS5StreamRequest(context.Background(), "alice-token"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 (the error arrives inside the stream)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"error":{"message":"mock generation failed"`) {
		t.Fatalf("error event missing from the client stream: %q", rec.Body.String())
	}
	if rec.Body.String() != chatPS5WantStream(true) {
		t.Fatalf("stream differs from the PS5 error stream\ngot:  %q\nwant: %q", rec.Body.String(), chatPS5WantStream(true))
	}
}

func TestChatPS5DisconnectWhileQueuedRemovesJob(t *testing.T) {
	mock := newMockPS5(t)
	release := make(chan struct{})
	open := sync.OnceFunc(func() { close(release) })
	inference, stand := chatPS5Inference(mock.inferenceRoutes(), release)
	defer inference.Close()
	defer open()
	gateway := testPS5Gateway(t, mock, inference)
	if state := gateway.ps5.health.Poll(context.Background()); !state.Online {
		t.Fatalf("PS5 not online: %+v", state)
	}
	handler := gateway.Handler()

	bobDone := make(chan struct{})
	go func() {
		defer close(bobDone)
		handler.ServeHTTP(httptest.NewRecorder(), chatPS5StreamRequest(context.Background(), "bob-token"))
	}()
	<-stand.entered

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	aliceDone := make(chan struct{})
	go func() {
		defer close(aliceDone)
		handler.ServeHTTP(httptest.NewRecorder(), chatPS5StreamRequest(ctx, "alice-token"))
	}()
	chatPS5Wait(t, "alice's job to be queued", func() bool {
		s := chatPS5Status(t, handler, "alice-token")
		return s.QueueLength == 1 && len(s.Jobs) == 1
	})

	// The client goes away while its chat is still waiting. The handler must return and drop the job.
	cancel()
	<-aliceDone
	status := chatPS5Status(t, handler, "alice-token")
	if status.QueueLength != 0 || len(status.Jobs) != 0 {
		t.Fatalf("job still listed after the client disconnected: %+v", status)
	}
	if len(stand.entered) != 0 {
		t.Fatal("abandoned chat reached the PS5 inference stand-in")
	}

	open()
	<-bobDone
}

// chatPS5SetEngine sets the mock's engine, as if the PS5 were already in that mode.
func chatPS5SetEngine(mock *mockPS5, engine string) {
	mock.mu.Lock()
	defer mock.mu.Unlock()
	mock.engine = engine
}

func TestChatPS5WaitsForEngineSwitch(t *testing.T) {
	mediaRunnerSetTimers(t, 2*time.Millisecond, mediaSwitchTimeout)
	mock := newMockPS5(t)
	chatPS5SetEngine(mock, mediaEngine)
	// After the switch to chat, the first two control probes report not ready, so the chat must keep waiting.
	var chatProbes atomic.Int32
	routes := mock.controlRoutes()
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, engine := mock.snapshot(); r.Method == http.MethodGet && r.URL.Path == "/v1/nexus/status" && engine == "llm" {
			if chatProbes.Add(1) <= 2 {
				mockPS5Write(w, http.StatusOK, "application/json", mockPS5Patch("control_status.json", map[string]any{"engine": "llm", "ready": false}))
				return
			}
		}
		routes.ServeHTTP(w, r)
	}))
	mock.Control.Close()
	mock.Control = control

	inference, _ := chatPS5Inference(mock.inferenceRoutes(), nil)
	defer inference.Close()
	gateway := testPS5Gateway(t, mock, inference)
	if state := gateway.ps5.health.Poll(context.Background()); !state.Online || state.Engine != mediaEngine {
		t.Fatalf("PS5 not online in media mode: %+v", state)
	}

	response := httptest.NewRecorder()
	gateway.Handler().ServeHTTP(response, chatPS5StreamRequest(context.Background(), "alice-token"))
	if response.Code != http.StatusOK || response.Body.String() != chatPS5WantStream(false) {
		t.Fatalf("chat during switch: status=%d body=%q", response.Code, response.Body.String())
	}
	if _, engine := mock.snapshot(); engine != "llm" {
		t.Fatalf("mock engine = %q, want llm", engine)
	}
	if n := chatProbes.Load(); n < 3 {
		t.Fatalf("chat answered after %d chat-mode probes, want it to wait for readiness", n)
	}
}

func TestChatPS5OfflineDuringSwitch(t *testing.T) {
	mediaRunnerSetTimers(t, 2*time.Millisecond, mediaSwitchTimeout)
	mediaRunnerSetGrace(t, 150*time.Millisecond)
	mock := newMockPS5(t)
	chatPS5SetEngine(mock, mediaEngine)
	// The engine switch to chat is accepted, then the PS5 drops off and stays offline.
	control, _ := mediaRunnerControlDropsOff(t, mock, 0)
	mock.Control.Close()
	mock.Control = control

	inference, stand := chatPS5Inference(mock.inferenceRoutes(), nil)
	defer inference.Close()
	gateway := testPS5Gateway(t, mock, inference)
	if state := gateway.ps5.health.Poll(context.Background()); !state.Online {
		t.Fatalf("PS5 not online before the switch: %+v", state)
	}

	response := httptest.NewRecorder()
	start := time.Now()
	gateway.Handler().ServeHTTP(response, chatPS5StreamRequest(context.Background(), "alice-token"))
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("offline chat waited %v; the shortened offline grace should end the wait first", elapsed)
	}
	if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") != "30" ||
		strings.TrimSpace(response.Body.String()) != `{"error":{"message":"PS5 is offline"}}` {
		t.Fatalf("offline during switch: status=%d retry-after=%q body=%q", response.Code, response.Header().Get("Retry-After"), response.Body.String())
	}
	if len(stand.entered) != 0 {
		t.Fatal("chat reached the PS5 inference stand-in while the PS5 was offline")
	}
}
