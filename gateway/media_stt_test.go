package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// sttTestRecord counts transcription POSTs that reached the PS5 stand-in and keeps the last body.
type sttTestRecord struct {
	mu    sync.Mutex
	calls int
	body  []byte
}

func (rec *sttTestRecord) record(body []byte) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.calls++
	rec.body = body
}

func (rec *sttTestRecord) snapshot() (int, []byte) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.calls, rec.body
}

// sttTestGateway wires a gateway to mock's control API and to a media server that records transcription calls.
// fail, when set, answers transcription requests in place of the mock.
func sttTestGateway(t *testing.T, mock *mockPS5, fail http.HandlerFunc) (*Gateway, *sttTestRecord) {
	t.Helper()
	rec := &sttTestRecord{}
	routes := mock.mediaRoutes()
	media := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/audio/transcriptions" {
			body, _ := io.ReadAll(r.Body)
			rec.record(body)
			if fail != nil {
				fail(w, r)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		routes.ServeHTTP(w, r)
	}))
	t.Cleanup(media.Close)

	credentials := t.TempDir()
	if err := os.WriteFile(filepath.Join(credentials, "ps5-token"), []byte(mockPS5Token), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CREDENTIALS_DIRECTORY", credentials)
	upstream, _ := url.Parse(mock.Inference.URL)
	upstream.Path = "/v1"
	g := NewGateway(Config{
		SubjectPermissions:  map[string]map[string]bool{"alice": {quotaPermissionAudio: true}, "bob": {permissionChat: true}},
		Upstream:            upstream,
		UpstreamBearerToken: mockPS5Token,
		RequestTimeout:      5 * time.Second,
		MaximumConcurrentAI: 1,
		PS5ControlURL:       mock.Control.URL,
		PS5MediaURL:         media.URL,
		PS5TokenCredential:  "ps5-token",
		PS5PollInterval:     time.Second,
		Quotas:              quotaPolicy{quotaPermissionAudio: {Unit: quotaUnitSeconds, Limit: 10}},
	}, nil)
	if state := g.ps5.health.Poll(context.Background()); !state.Online {
		t.Fatalf("PS5 not online: %+v", state)
	}
	return g, rec
}

// sttTestWAV returns base64 of PCM16 mono 16 kHz silence with the given number of samples.
func sttTestWAV(samples int) string {
	var b bytes.Buffer
	b.WriteString("RIFF")
	_ = binary.Write(&b, binary.LittleEndian, uint32(36+samples*2))
	b.WriteString("WAVEfmt ")
	_ = binary.Write(&b, binary.LittleEndian, uint32(16))
	_ = binary.Write(&b, binary.LittleEndian, struct {
		Format, Channels uint16
		Rate, ByteRate   uint32
		Align, Bits      uint16
	}{1, 1, 16000, 32000, 2, 16})
	b.WriteString("data")
	_ = binary.Write(&b, binary.LittleEndian, uint32(samples*2))
	b.Write(make([]byte, samples*2))
	return base64.StdEncoding.EncodeToString(b.Bytes())
}

func sttTestRequest(subject string, body []byte) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", bytes.NewReader(body))
	return r.WithContext(context.WithValue(r.Context(), principalKey{}, subject))
}

func sttTestBody(t *testing.T, fields map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestSTTHappyPathReturnsOnlyTextLanguageSegments(t *testing.T) {
	mock := newMockPS5(t)
	g, rec := sttTestGateway(t, mock, nil)
	body := sttTestBody(t, map[string]any{"audio_b64": sttTestWAV(160), "language": "en", "model": "evil/other.bin"})

	response := httptest.NewRecorder()
	g.handleTranscription()(response, sttTestRequest("alice", body))
	if response.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", response.Code, response.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got["text"] != "mock transcript" || got["language"] != "en" {
		t.Fatalf("response = %v, want exactly text, language and segments", got)
	}
	segments, ok := got["segments"].([]any)
	if !ok || len(segments) != 1 {
		t.Fatalf("segments = %v", got["segments"])
	}
	if seg, ok := segments[0].(map[string]any); !ok || len(seg) != 3 || seg["text"] != "mock transcript" {
		t.Fatalf("segment = %v, want exactly start, end and text", segments[0])
	}

	calls, sent := rec.snapshot()
	if calls != 1 {
		t.Fatalf("PS5 transcription calls = %d, want 1", calls)
	}
	var forwarded map[string]any
	if err := json.Unmarshal(sent, &forwarded); err != nil {
		t.Fatal(err)
	}
	if forwarded["model"] != sttModel {
		t.Fatalf("forwarded model = %v, want the server default; the client's model must be ignored", forwarded["model"])
	}
}

func TestSTTRejectsBadRequestsBeforeQueueing(t *testing.T) {
	cases := map[string][]byte{
		"not a WAV":        sttTestBody(t, map[string]any{"audio_b64": base64.StdEncoding.EncodeToString([]byte("hello, not audio"))}),
		"over 60 seconds":  sttTestBody(t, map[string]any{"audio_b64": sttTestWAV(61 * 16000)}),
		"bad base64":       sttTestBody(t, map[string]any{"audio_b64": "%%%not-base64%%%"}),
		"missing audio":    []byte(`{}`),
		"bad language tag": sttTestBody(t, map[string]any{"audio_b64": sttTestWAV(160), "language": "e n"}),
		"not JSON":         []byte(`{"audio_b64":`),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			mock := newMockPS5(t)
			g, rec := sttTestGateway(t, mock, nil)
			response := httptest.NewRecorder()
			g.handleTranscription()(response, sttTestRequest("alice", body))
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400: %s", response.Code, response.Body.String())
			}
			if calls, _ := rec.snapshot(); calls != 0 {
				t.Fatalf("invalid request reached the PS5 %d times", calls)
			}
			// Nothing was queued or charged, so the full daily allowance remains.
			if remaining, err := g.quota.Consume("alice", quotaPermissionAudio, 1); err != nil || remaining != 9 {
				t.Fatalf("remaining quota = %d, err = %v, want 9", remaining, err)
			}
		})
	}
}

func TestSTTPermissionDeniedBeforeAnything(t *testing.T) {
	mock := newMockPS5(t)
	g, rec := sttTestGateway(t, mock, nil)
	body := sttTestBody(t, map[string]any{"audio_b64": sttTestWAV(160)})
	for _, subject := range []string{"bob", ""} {
		response := httptest.NewRecorder()
		g.handleTranscription()(response, sttTestRequest(subject, body))
		if response.Code != http.StatusForbidden {
			t.Fatalf("subject %q: status %d, want 403", subject, response.Code)
		}
	}
	if calls, _ := rec.snapshot(); calls != 0 {
		t.Fatalf("denied requests reached the PS5 %d times", calls)
	}
}

func TestSTTQuotaIsChargedInWholeSeconds(t *testing.T) {
	mock := newMockPS5(t)
	g, rec := sttTestGateway(t, mock, nil)

	// 2.5 seconds costs 3 of the 10 daily seconds, rounded up.
	response := httptest.NewRecorder()
	g.handleTranscription()(response, sttTestRequest("alice", sttTestBody(t, map[string]any{"audio_b64": sttTestWAV(40000)})))
	if response.Code != http.StatusOK {
		t.Fatalf("2.5 s clip: status %d: %s", response.Code, response.Body.String())
	}
	if remaining, err := g.quota.Consume("alice", quotaPermissionAudio, 1); err != nil || remaining != 6 {
		t.Fatalf("remaining after 2.5 s clip = %d, err = %v, want 6", remaining, err)
	}

	// Six seconds are left after the clip and the probe above, so an 8 s clip is refused without a PS5 call.
	response = httptest.NewRecorder()
	g.handleTranscription()(response, sttTestRequest("alice", sttTestBody(t, map[string]any{"audio_b64": sttTestWAV(8 * 16000)})))
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("8 s clip over quota: status %d, want 429", response.Code)
	}
	if calls, _ := rec.snapshot(); calls != 1 {
		t.Fatalf("PS5 transcription calls = %d, want 1", calls)
	}
}

func TestSTTPS5ErrorIsSanitized(t *testing.T) {
	mock := newMockPS5(t)
	fail := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"internal ` + mockPS5Token + ` detail"}}`))
	}
	g, _ := sttTestGateway(t, mock, fail)
	response := httptest.NewRecorder()
	g.handleTranscription()(response, sttTestRequest("alice", sttTestBody(t, map[string]any{"audio_b64": sttTestWAV(160)})))
	if response.Code != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", response.Code)
	}
	if strings.Contains(response.Body.String(), mockPS5Token) || strings.Contains(response.Body.String(), "detail") {
		t.Fatalf("PS5 error text leaked to the client: %s", response.Body.String())
	}
	if want := `{"error":{"message":"PS5 returned Internal Server Error"}}`; strings.TrimSpace(response.Body.String()) != want {
		t.Fatalf("body = %s, want %s", response.Body.String(), want)
	}
}
