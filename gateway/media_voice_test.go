package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// voiceCapture keeps each voice request body the PS5 media API received.
type voiceCapture struct {
	mu     sync.Mutex
	bodies [][]byte
}

func (c *voiceCapture) add(raw []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bodies = append(c.bodies, raw)
}

func (c *voiceCapture) last(t *testing.T) map[string]any {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.bodies) == 0 {
		t.Fatal("PS5 received no voice request")
	}
	var got map[string]any
	if err := json.Unmarshal(c.bodies[len(c.bodies)-1], &got); err != nil {
		t.Fatalf("PS5 voice body is not JSON: %v", err)
	}
	return got
}

// voiceMediaServer serves the mock media API and records voice request bodies.
func voiceMediaServer(t *testing.T, mock *mockPS5, capture *voiceCapture) *httptest.Server {
	t.Helper()
	routes := mock.mediaRoutes()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/voice/chat" {
			raw, _ := io.ReadAll(r.Body)
			capture.add(raw)
			r.Body = io.NopCloser(bytes.NewReader(raw))
		}
		routes.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// voiceTestGateway wires a gateway to the mock PS5 with ai.voice and ai.voice.clone for alice, and a usage log in dir.
func voiceTestGateway(t *testing.T, mock *mockPS5, mediaURL, usageDir string) *Gateway {
	t.Helper()
	mediaRunnerSetTimers(t, 2*time.Millisecond, mediaSwitchTimeout)
	inference := httptest.NewServer(mock.inferenceRoutes())
	t.Cleanup(inference.Close)
	g := mediaRunnerGateway(t, mock.Control.URL, mediaURL, inference.URL, 10)
	g.cfg.SubjectPermissions = map[string]map[string]bool{"alice": {quotaPermissionVoice: true, quotaPermissionVoiceClone: true}}
	g.quota = newQuotaMeter(quotaPolicy{quotaPermissionVoice: {Unit: quotaUnitSeconds, Limit: 100000}}, time.Now)
	usage, err := newUsageLog(usageDir, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	g.usage = usage
	if s := g.ps5.health.Poll(context.Background()); !s.Online {
		t.Fatalf("PS5 not online: %+v", s)
	}
	return g
}

// voiceBody builds a client body with the mock WAV as audio and extra JSON fields appended.
func voiceBody(t *testing.T, extra string) string {
	t.Helper()
	audio := base64.StdEncoding.EncodeToString(mockPS5WAVBytes())
	if extra != "" {
		extra = "," + extra
	}
	return `{"audio_b64":"` + audio + `"` + extra + `}`
}

func voiceRequest(body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/voice/chat", strings.NewReader(body))
	return r.WithContext(context.WithValue(r.Context(), principalKey{}, "alice"))
}

func voiceUsageLines(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var all strings.Builder
	for _, e := range entries {
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		all.Write(raw)
	}
	return all.String()
}

func TestVoiceForwardsEventsInOrder(t *testing.T) {
	mock := newMockPS5(t)
	capture := &voiceCapture{}
	media := voiceMediaServer(t, mock, capture)
	g := voiceTestGateway(t, mock, media.URL, t.TempDir())

	rec := httptest.NewRecorder()
	g.handleVoiceChat()(rec, voiceRequest(voiceBody(t, "")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}
	var want strings.Builder
	for _, e := range mockPS5Events("media_voice_events.json") {
		want.WriteString("data: " + mockPS5Compact(e) + "\n\n")
	}
	want.WriteString("data: [DONE]\n\n")
	if got := rec.Body.String(); got != want.String() {
		t.Fatalf("forwarded stream differs:\ngot:  %s\nwant: %s", got, want.String())
	}
}

func TestVoiceClientSystemPromptIsNotForwarded(t *testing.T) {
	mock := newMockPS5(t)
	capture := &voiceCapture{}
	media := voiceMediaServer(t, mock, capture)
	g := voiceTestGateway(t, mock, media.URL, t.TempDir())

	body := voiceBody(t, `"system":"client system override","messages":[{"role":"system","content":"client system marker"},{"role":"user","content":"hi"}]`)
	rec := httptest.NewRecorder()
	g.handleVoiceChat()(rec, voiceRequest(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	got := capture.last(t)
	if got["system"] != mediaVoiceSystemPrompt {
		t.Fatalf("PS5 system = %v, want the fixed server prompt", got["system"])
	}
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), "client system") {
		t.Fatalf("client system text reached PS5: %s", raw)
	}
	msgs, _ := got["messages"].([]any)
	if len(msgs) != 1 || msgs[0].(map[string]any)["role"] != "user" {
		t.Fatalf("PS5 messages = %v, want only the user turn", msgs)
	}
}

func TestVoiceHistoryTrimmedTo20AndBoundedInBytes(t *testing.T) {
	mock := newMockPS5(t)
	capture := &voiceCapture{}
	media := voiceMediaServer(t, mock, capture)
	g := voiceTestGateway(t, mock, media.URL, t.TempDir())

	var msgs []string
	for i := 0; i < 25; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, `{"role":"`+role+`","content":"m`+string(rune('a'+i))+`"}`)
	}
	rec := httptest.NewRecorder()
	g.handleVoiceChat()(rec, voiceRequest(voiceBody(t, `"messages":[`+strings.Join(msgs, ",")+`]`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	got, _ := capture.last(t)["messages"].([]any)
	if len(got) != 20 {
		t.Fatalf("PS5 history length = %d, want 20", len(got))
	}
	if first := got[0].(map[string]any)["content"]; first != "mf" {
		t.Fatalf("oldest kept message = %v, want the 6th turn (mf)", first)
	}

	// Three 10 KiB turns exceed the 16 KiB history budget, so only the newest one survives.
	big := strings.Repeat("x", 10<<10)
	body := voiceBody(t, `"messages":[{"role":"user","content":"`+big+`"},{"role":"assistant","content":"`+big+`"},{"role":"user","content":"`+big+`"}]`)
	rec = httptest.NewRecorder()
	g.handleVoiceChat()(rec, voiceRequest(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	got, _ = capture.last(t)["messages"].([]any)
	if len(got) != 1 {
		t.Fatalf("byte-bounded history length = %d, want 1", len(got))
	}
}

func TestVoiceErrorEventSurfacedAndOutcomeFailed(t *testing.T) {
	mock := newMockPS5(t)
	mock.Set(func(s *mockPS5Switches) { s.ErrorEvent = true })
	defer mock.Set(func(s *mockPS5Switches) { *s = mockPS5Switches{} })
	capture := &voiceCapture{}
	media := voiceMediaServer(t, mock, capture)
	usageDir := t.TempDir()
	g := voiceTestGateway(t, mock, media.URL, usageDir)

	rec := httptest.NewRecorder()
	g.handleVoiceChat()(rec, voiceRequest(voiceBody(t, "")))
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, body)
	}
	errEvent := "data: " + mockPS5Compact(mockPS5Read("media_voice_error_event.json")) + "\n\n"
	if !strings.Contains(body, errEvent) {
		t.Fatalf("error event not forwarded: %s", body)
	}
	if !strings.HasSuffix(body, "data: [DONE]\n\n") || strings.Contains(body, `{"error":`) {
		t.Fatalf("stream should end with [DONE] and no JSON error body: %s", body)
	}
	if lines := voiceUsageLines(t, usageDir); !strings.Contains(lines, `"outcome":"failed"`) || !strings.Contains(lines, "/v1/voice/chat") {
		t.Fatalf("usage outcome not failed: %s", lines)
	}
}

func TestVoiceClientDisconnectCancelsPS5(t *testing.T) {
	mock := newMockPS5(t)
	canceled := make(chan struct{})
	routes := mock.mediaRoutes()
	media := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/voice/chat" {
			routes.ServeHTTP(w, r)
			return
		}
		// Send one event, then hold the stream open until the gateway cancels the PS5 request.
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"status\":\"loading\"}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			close(canceled)
		case <-time.After(10 * time.Second):
		}
	}))
	t.Cleanup(media.Close)
	g := voiceTestGateway(t, mock, media.URL, t.TempDir())

	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.handleVoiceChat()(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, "alice")))
	}))
	t.Cleanup(gateway.Close)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gateway.URL+"/v1/voice/chat", strings.NewReader(voiceBody(t, "")))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "data:") {
		t.Fatalf("first event = %q, err = %v", line, err)
	}
	cancel()
	resp.Body.Close()

	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("client disconnect did not cancel the PS5 voice request")
	}
}
