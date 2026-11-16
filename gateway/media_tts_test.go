package main

import (
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
	"sync/atomic"
	"testing"
	"time"
)

// ttsSpy counts and records the requests that reach the PS5 speech route.
type ttsSpy struct {
	hits atomic.Int32
	mu   sync.Mutex
	body []byte
}

func (s *ttsSpy) lastBody() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.body
}

// ttsInstallSpy replaces the mock's media server with one that records speech requests and answers them with
// speech. Every other media route still goes to the mock, so health and the engine switch keep working.
func ttsInstallSpy(mock *mockPS5, speech func(w http.ResponseWriter)) *ttsSpy {
	spy := &ttsSpy{}
	orig := mock.mediaRoutes()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/speech" {
			orig.ServeHTTP(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		spy.hits.Add(1)
		spy.mu.Lock()
		spy.body = raw
		spy.mu.Unlock()
		speech(w)
	}))
	mock.Media.Close()
	mock.Media = srv
	return spy
}

func ttsWAVSpeech(w http.ResponseWriter) {
	mockPS5Write(w, http.StatusOK, "audio/wav", mockPS5WAVBytes())
}

// ttsGateway wires the handler to the mock PS5 with the given subject permissions, one audio quota, and a usage log.
func ttsGateway(t *testing.T, mock *mockPS5, subjects map[string]map[string]bool) (*Gateway, string) {
	t.Helper()
	inference := httptest.NewServer(mock.inferenceRoutes())
	t.Cleanup(inference.Close)
	g := testPS5Gateway(t, mock, inference)
	g.cfg.SubjectPermissions = subjects
	g.quota = newQuotaMeter(quotaPolicy{quotaPermissionAudio: {Unit: quotaUnitRequests, Limit: 100}}, time.Now)
	usageDir := t.TempDir()
	usage, err := newUsageLog(usageDir, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	g.usage = usage
	if state := g.ps5.health.Poll(context.Background()); !state.Online {
		t.Fatalf("PS5 not online for speech: %+v", state)
	}
	return g, usageDir
}

func ttsPost(g *Gateway, subject, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", strings.NewReader(body))
	r = r.WithContext(context.WithValue(r.Context(), principalKey{}, subject))
	w := httptest.NewRecorder()
	g.handleSpeech().ServeHTTP(w, r)
	return w
}

// ttsUsageText returns every usage line written to dir.
func ttsUsageText(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, e := range entries {
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		b.Write(raw)
	}
	return b.String()
}

// ttsReference is a valid 10 ms WAV reference voice, base64 encoded, with a marker no usage line may hold.
func ttsReference() string {
	return base64.StdEncoding.EncodeToString(mockPS5WAVBytes())
}

func TestTTSHappyPath(t *testing.T) {
	mock := newMockPS5(t)
	spy := ttsInstallSpy(mock, ttsWAVSpeech)
	g, usageDir := ttsGateway(t, mock, map[string]map[string]bool{"alice": {quotaPermissionAudio: true}})

	response := ttsPost(g, "alice", `{"input":"Hello there.","language":"en"}`)
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "audio/wav" {
		t.Fatalf("speech: status=%d content-type=%q body=%q", response.Code, response.Header().Get("Content-Type"), response.Body.String())
	}
	if !ttsIsWAV(response.Body.Bytes()) {
		t.Fatal("speech response is not a WAV")
	}
	var sent ttsPS5Body
	if err := json.Unmarshal(spy.lastBody(), &sent); err != nil {
		t.Fatal(err)
	}
	if sent.ResponseFormat != "wav" || sent.Input != "Hello there." || sent.Language != "en" || sent.SpeakerB64 != "" || sent.Model != ttsModel {
		t.Fatalf("PS5 body = %+v", sent)
	}
	if usage := ttsUsageText(t, usageDir); !strings.Contains(usage, `"route":"/v1/audio/speech"`) || strings.Contains(usage, "#clone") {
		t.Fatalf("usage line = %s", usage)
	}
}

func TestTTSCloneSendsSpeakerWithoutStoringIt(t *testing.T) {
	mock := newMockPS5(t)
	spy := ttsInstallSpy(mock, ttsWAVSpeech)
	g, usageDir := ttsGateway(t, mock, map[string]map[string]bool{
		"alice": {quotaPermissionAudio: true, quotaPermissionVoiceClone: true},
	})
	ref := ttsReference()

	body := `{"input":"Hello there.","voice_reference_b64":"` + ref + `","consent":true}`
	response := ttsPost(g, "alice", body)
	if response.Code != http.StatusOK || !ttsIsWAV(response.Body.Bytes()) {
		t.Fatalf("clone speech: status=%d body=%q", response.Code, response.Body.String())
	}
	var sent ttsPS5Body
	if err := json.Unmarshal(spy.lastBody(), &sent); err != nil {
		t.Fatal(err)
	}
	if sent.SpeakerB64 != ref || sent.ResponseFormat != "wav" {
		t.Fatalf("PS5 body does not carry the reference voice as speaker_b64")
	}
	usage := ttsUsageText(t, usageDir)
	if !strings.Contains(usage, `"route":"/v1/audio/speech#clone"`) {
		t.Fatalf("clone usage line lacks the clone flag: %s", usage)
	}
	if strings.Contains(usage, ref) {
		t.Fatal("usage log holds the reference audio")
	}
}

func TestTTSCloneWithoutVoiceCloneIsForbidden(t *testing.T) {
	mock := newMockPS5(t)
	spy := ttsInstallSpy(mock, ttsWAVSpeech)
	g, _ := ttsGateway(t, mock, map[string]map[string]bool{"alice": {quotaPermissionAudio: true}})

	response := ttsPost(g, "alice", `{"input":"Hi","voice_reference_b64":"`+ttsReference()+`","consent":true}`)
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "ai.voice.clone") {
		t.Fatalf("clone without ai.voice.clone: status=%d body=%q", response.Code, response.Body.String())
	}
	if spy.hits.Load() != 0 {
		t.Fatal("refused clone reached the PS5 speech route")
	}
	if _, engine := mock.snapshot(); engine != "llm" {
		t.Fatalf("refused clone switched the PS5 engine to %q", engine)
	}
}

func TestTTSCloneWithoutConsentIsForbidden(t *testing.T) {
	mock := newMockPS5(t)
	spy := ttsInstallSpy(mock, ttsWAVSpeech)
	g, _ := ttsGateway(t, mock, map[string]map[string]bool{
		"alice": {quotaPermissionAudio: true, quotaPermissionVoiceClone: true},
	})

	response := ttsPost(g, "alice", `{"input":"Hi","voice_reference_b64":"`+ttsReference()+`","consent":false}`)
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "consent") {
		t.Fatalf("clone without consent: status=%d body=%q", response.Code, response.Body.String())
	}
	if spy.hits.Load() != 0 {
		t.Fatal("clone without consent reached the PS5 speech route")
	}
	if _, engine := mock.snapshot(); engine != "llm" {
		t.Fatalf("clone without consent switched the PS5 engine to %q", engine)
	}
}

func TestTTSOversizeInputRejectedBeforeQueue(t *testing.T) {
	mock := newMockPS5(t)
	spy := ttsInstallSpy(mock, ttsWAVSpeech)
	g, _ := ttsGateway(t, mock, map[string]map[string]bool{"alice": {quotaPermissionAudio: true}})

	response := ttsPost(g, "alice", `{"input":"`+strings.Repeat("a", mediaValMaxTTSChars+1)+`"}`)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("oversize input: status=%d body=%q", response.Code, response.Body.String())
	}
	if spy.hits.Load() != 0 {
		t.Fatal("oversize input reached the PS5 speech route")
	}
	if _, engine := mock.snapshot(); engine != "llm" {
		t.Fatalf("oversize input switched the PS5 engine to %q", engine)
	}
	if g.queue.running != nil || len(g.queue.queued) != 0 {
		t.Fatal("oversize input was queued")
	}
}

func TestTTSNonWAVUpstreamIs502(t *testing.T) {
	mock := newMockPS5(t)
	spy := ttsInstallSpy(mock, func(w http.ResponseWriter) {
		mockPS5Write(w, http.StatusOK, "application/json", []byte(`{"audio_b64":"bm90IGF1ZGlv"}`))
	})
	g, _ := ttsGateway(t, mock, map[string]map[string]bool{"alice": {quotaPermissionAudio: true}})

	response := ttsPost(g, "alice", `{"input":"Hello there."}`)
	if response.Code != http.StatusBadGateway || !strings.Contains(response.Body.String(), "PS5 returned no WAV audio") {
		t.Fatalf("non-WAV upstream: status=%d body=%q", response.Code, response.Body.String())
	}
	if spy.hits.Load() != 1 {
		t.Fatalf("speech requests reaching the PS5 = %d, want 1", spy.hits.Load())
	}
}
