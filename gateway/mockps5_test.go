package main

// Test-only stand-in for the PS5 (plan §1.6): inference :8081, control :8082, media :8083.
// Bodies come from testdata/ps5 (shapes in plan §1.3 and native/nexus_control.cpp); see testdata/ps5/README.md.

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
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

// mockPS5Token is a fake bearer; every route except the media /health requires it.
const mockPS5Token = "test-token-0123456789abcdef0123456789abcdef"

// mockPS5Switches are fault toggles a test flips while the mock runs.
type mockPS5Switches struct {
	Offline    bool          // every route answers 503 (media /health answers its unavailable body)
	Busy       bool          // generation and control-mutation routes answer 409
	Slow       time.Duration // delay before each response; gives up when the client does
	ErrorEvent bool          // streaming routes end with an error event in place of the last event, then [DONE]
}

// mockPS5 is the three-server PS5 stand-in.
type mockPS5 struct {
	Inference *httptest.Server // :8081 llama-server, chat
	Control   *httptest.Server // :8082 control API
	Media     *httptest.Server // :8083 media API

	mu     sync.Mutex
	sw     mockPS5Switches
	engine string
}

// mockPS5Errors names the fixture for each failure body of one server.
type mockPS5Errors struct {
	public       bool // skip the bearer check
	offline      string
	busy         string
	unauthorized string
}

var (
	mockPS5PSllamaErrors = mockPS5Errors{offline: "error_offline.json", busy: "error_busy.json", unauthorized: "error_unauthorized.json"}
	mockPS5ControlErrors = mockPS5Errors{offline: "control_error_offline.json", busy: "control_error_busy.json", unauthorized: "control_error_unauthorized.json"}
)

func newMockPS5(t *testing.T) *mockPS5 {
	t.Helper()
	m := &mockPS5{engine: "llm"}
	m.Inference = httptest.NewServer(m.inferenceRoutes())
	m.Control = httptest.NewServer(m.controlRoutes())
	m.Media = httptest.NewServer(m.mediaRoutes())
	t.Cleanup(func() {
		m.Inference.Close()
		m.Control.Close()
		m.Media.Close()
	})
	return m
}

// Set changes the switches; safe while requests are in flight.
func (m *mockPS5) Set(change func(*mockPS5Switches)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	change(&m.sw)
}

func (m *mockPS5) snapshot() (mockPS5Switches, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sw, m.engine
}

// admit applies slow, offline, bearer and busy checks in that order. It writes the failure
// itself and returns false when the handler must stop.
func (m *mockPS5) admit(w http.ResponseWriter, r *http.Request, errs mockPS5Errors, busy bool) bool {
	sw, _ := m.snapshot()
	if sw.Slow > 0 {
		select {
		case <-time.After(sw.Slow):
		case <-r.Context().Done():
			return false
		}
	}
	if sw.Offline {
		mockPS5Send(w, http.StatusServiceUnavailable, errs.offline)
		return false
	}
	if !errs.public && r.Header.Get("Authorization") != "Bearer "+mockPS5Token {
		mockPS5Send(w, http.StatusUnauthorized, errs.unauthorized)
		return false
	}
	if busy && sw.Busy {
		mockPS5Send(w, http.StatusConflict, errs.busy)
		return false
	}
	return true
}

func (m *mockPS5) inferenceRoutes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		if m.admit(w, r, mockPS5PSllamaErrors, false) {
			mockPS5Send(w, http.StatusOK, "inference_models.json")
		}
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if !m.admit(w, r, mockPS5PSllamaErrors, true) {
			return
		}
		var req struct {
			Messages []json.RawMessage `json:"messages"`
			Stream   bool              `json:"stream"`
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil || len(req.Messages) == 0 {
			mockPS5Send(w, http.StatusUnprocessableEntity, "error_invalid.json")
			return
		}
		if !req.Stream {
			mockPS5Send(w, http.StatusOK, "inference_chat.json")
			return
		}
		sw, _ := m.snapshot()
		mockPS5SSE(w, mockPS5Events("inference_chat_stream.json"), mockPS5Read("inference_error_event.json"), sw.ErrorEvent)
	})
	return mux
}

func (m *mockPS5) controlRoutes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/nexus/status", func(w http.ResponseWriter, r *http.Request) {
		if !m.admit(w, r, mockPS5ControlErrors, false) {
			return
		}
		_, engine := m.snapshot()
		mockPS5Write(w, http.StatusOK, "application/json", mockPS5Patch("control_status.json", map[string]any{"engine": engine}))
	})
	mux.HandleFunc("GET /v1/nexus/models", func(w http.ResponseWriter, r *http.Request) {
		if m.admit(w, r, mockPS5ControlErrors, false) {
			mockPS5Send(w, http.StatusOK, "control_models.json")
		}
	})
	mux.HandleFunc("POST /v1/nexus/models/unload", func(w http.ResponseWriter, r *http.Request) {
		if m.admit(w, r, mockPS5ControlErrors, true) {
			mockPS5Send(w, http.StatusAccepted, "control_accepted.json")
		}
	})
	mux.HandleFunc("POST /v1/nexus/models/{id}/load", func(w http.ResponseWriter, r *http.Request) {
		if !m.admit(w, r, mockPS5ControlErrors, true) {
			return
		}
		if !mockPS5HasModel(r.PathValue("id")) {
			mockPS5Send(w, http.StatusNotFound, "control_unknown_model.json")
			return
		}
		mockPS5Send(w, http.StatusAccepted, "control_accepted.json")
	})
	for _, engine := range []string{"llm", "media"} {
		mux.HandleFunc("POST /v1/nexus/engine/"+engine, func(w http.ResponseWriter, r *http.Request) {
			if !m.admit(w, r, mockPS5ControlErrors, true) {
				return
			}
			m.mu.Lock()
			m.engine = engine // the real switch completes later; the mock flips at once
			m.mu.Unlock()
			mockPS5Send(w, http.StatusAccepted, "control_accepted.json")
		})
	}
	return mux
}

func (m *mockPS5) mediaRoutes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		errs := mockPS5PSllamaErrors
		errs.public = true
		errs.offline = "media_health_unavailable.json"
		if m.admit(w, r, errs, false) {
			mockPS5Send(w, http.StatusOK, "media_health.json")
		}
	})
	mux.HandleFunc("POST /v1/images/generations", func(w http.ResponseWriter, r *http.Request) {
		if !m.admit(w, r, mockPS5PSllamaErrors, true) {
			return
		}
		var req struct {
			Prompt string `json:"prompt"`
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil || req.Prompt == "" {
			mockPS5Send(w, http.StatusUnprocessableEntity, "error_invalid.json")
			return
		}
		var tpl struct{ Width, Height int }
		_ = json.Unmarshal(mockPS5Read("media_image.json"), &tpl)
		mockPS5Write(w, http.StatusOK, "application/json", mockPS5Patch("media_image.json", map[string]any{
			"data": []map[string]string{{"b64_json": mockPS5PNG(tpl.Width, tpl.Height)}},
		}))
	})
	mux.HandleFunc("POST /v1/videos/generations", func(w http.ResponseWriter, r *http.Request) {
		if !m.admit(w, r, mockPS5PSllamaErrors, true) {
			return
		}
		var req struct {
			Prompt string `json:"prompt"`
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil || req.Prompt == "" {
			mockPS5Send(w, http.StatusUnprocessableEntity, "error_invalid.json")
			return
		}
		var tpl struct{ Width, Height, Frames int }
		_ = json.Unmarshal(mockPS5Read("media_video.json"), &tpl)
		frames := make([]map[string]string, 0, tpl.Frames)
		for i := 0; i < tpl.Frames; i++ {
			frames = append(frames, map[string]string{"b64_png": mockPS5PNG(tpl.Width, tpl.Height)})
		}
		mockPS5Write(w, http.StatusOK, "application/json", mockPS5Patch("media_video.json", map[string]any{"data": frames}))
	})
	mux.HandleFunc("POST /v1/audio/transcriptions", func(w http.ResponseWriter, r *http.Request) {
		if !m.admit(w, r, mockPS5PSllamaErrors, true) {
			return
		}
		if !mockPS5ReadWAV(r) {
			mockPS5Send(w, http.StatusUnprocessableEntity, "error_invalid.json")
			return
		}
		mockPS5Send(w, http.StatusOK, "media_transcription.json")
	})
	mux.HandleFunc("POST /v1/audio/speech", func(w http.ResponseWriter, r *http.Request) {
		if !m.admit(w, r, mockPS5PSllamaErrors, true) {
			return
		}
		var req struct {
			Input          string `json:"input"`
			ResponseFormat string `json:"response_format"`
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil || req.Input == "" {
			mockPS5Send(w, http.StatusUnprocessableEntity, "error_invalid.json")
			return
		}
		if req.ResponseFormat == "wav" {
			mockPS5Write(w, http.StatusOK, "audio/wav", mockPS5WAVBytes())
			return
		}
		mockPS5Send(w, http.StatusOK, "media_speech.json")
	})
	mux.HandleFunc("POST /v1/voice/chat", func(w http.ResponseWriter, r *http.Request) {
		if !m.admit(w, r, mockPS5PSllamaErrors, true) {
			return
		}
		if !mockPS5ReadWAV(r) {
			mockPS5Send(w, http.StatusUnprocessableEntity, "error_invalid.json")
			return
		}
		sw, _ := m.snapshot()
		mockPS5SSE(w, mockPS5Events("media_voice_events.json"), mockPS5Read("media_voice_error_event.json"), sw.ErrorEvent)
	})
	return mux
}

// mockPS5SSE writes events as SSE data lines, then [DONE]. With inject set, the last event is
// replaced by errEvent, as PSllama ends a failed stream.
func mockPS5SSE(w http.ResponseWriter, events []json.RawMessage, errEvent []byte, inject bool) {
	if inject && len(events) > 0 {
		events = events[:len(events)-1]
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	send := func(data string) {
		fmt.Fprintf(w, "data: %s\n\n", data)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	for _, e := range events {
		send(mockPS5Compact(e))
	}
	if inject {
		send(mockPS5Compact(errEvent))
	}
	send("[DONE]")
}

func mockPS5Compact(raw []byte) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		panic(err)
	}
	return buf.String()
}

// mockPS5Read returns a fixture; the __WAV_B64__ placeholder becomes a generated WAV.
func mockPS5Read(name string) []byte {
	b, err := os.ReadFile(filepath.Join("testdata", "ps5", name))
	if err != nil {
		panic(err)
	}
	return []byte(strings.ReplaceAll(string(b), "__WAV_B64__", base64.StdEncoding.EncodeToString(mockPS5WAVBytes())))
}

func mockPS5Events(name string) []json.RawMessage {
	var events []json.RawMessage
	if err := json.Unmarshal(mockPS5Read(name), &events); err != nil {
		panic(err)
	}
	return events
}

// mockPS5Patch returns a fixture with some top-level fields replaced.
func mockPS5Patch(name string, set map[string]any) []byte {
	var doc map[string]any
	if err := json.Unmarshal(mockPS5Read(name), &doc); err != nil {
		panic(err)
	}
	for k, v := range set {
		doc[k] = v
	}
	out, err := json.Marshal(doc)
	if err != nil {
		panic(err)
	}
	return out
}

func mockPS5Write(w http.ResponseWriter, code int, ctype string, body []byte) {
	w.Header().Set("Content-Type", ctype)
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

func mockPS5Send(w http.ResponseWriter, code int, fixture string) {
	mockPS5Write(w, code, "application/json", mockPS5Read(fixture))
}

func mockPS5HasModel(id string) bool {
	var doc struct {
		Models []struct {
			ID   string `json:"id"`
			Fits bool   `json:"fits"`
		} `json:"models"`
	}
	_ = json.Unmarshal(mockPS5Read("control_models.json"), &doc)
	for _, m := range doc.Models {
		if m.ID == id && m.Fits {
			return true
		}
	}
	return false
}

// mockPS5WAVBytes is 10 ms of 16 kHz mono PCM16 silence.
func mockPS5WAVBytes() []byte {
	const samples = 160
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
	return b.Bytes()
}

func mockPS5IsWAV(raw []byte) bool {
	return len(raw) >= 12 && string(raw[:4]) == "RIFF" && string(raw[8:12]) == "WAVE"
}

// mockPS5ReadWAV reports whether the JSON body's audio_b64 decodes to a WAV.
func mockPS5ReadWAV(r *http.Request) bool {
	var req struct {
		AudioB64 string `json:"audio_b64"`
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil {
		return false
	}
	raw, err := base64.StdEncoding.DecodeString(req.AudioB64)
	return err == nil && mockPS5IsWAV(raw)
}

// mockPS5PNG returns a w x h PNG (gradient), base64 encoded.
func mockPS5PNG(w, h int) string {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 0x80, A: 0xff})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic(err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func mockPS5Call(t *testing.T, method, url, body, bearer string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, out
}

func mockPS5Expect(t *testing.T, got, want int, body []byte) {
	t.Helper()
	if got != want {
		t.Fatalf("status %d, want %d: %s", got, want, body)
	}
}

func TestMockPS5(t *testing.T) {
	m := newMockPS5(t)
	tok := mockPS5Token
	wavB64 := base64.StdEncoding.EncodeToString(mockPS5WAVBytes())
	audioBody := fmt.Sprintf(`{"audio_b64":%q}`, wavB64)
	chatBody := `{"messages":[{"role":"user","content":"hi"}]}`

	t.Run("fixtures", func(t *testing.T) {
		files, err := filepath.Glob(filepath.Join("testdata", "ps5", "*.json"))
		if err != nil || len(files) == 0 {
			t.Fatalf("no fixtures: %v", err)
		}
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			if !json.Valid(b) {
				t.Errorf("%s is not valid JSON", filepath.Base(f))
			}
		}
		if !mockPS5IsWAV(mockPS5WAVBytes()) {
			t.Fatal("generated WAV lacks RIFF/WAVE header")
		}
	})

	t.Run("inference", func(t *testing.T) {
		code, body := mockPS5Call(t, "GET", m.Inference.URL+"/v1/models", "", tok)
		mockPS5Expect(t, code, 200, body)
		code, body = mockPS5Call(t, "POST", m.Inference.URL+"/v1/chat/completions", chatBody, tok)
		mockPS5Expect(t, code, 200, body)
		code, body = mockPS5Call(t, "POST", m.Inference.URL+"/v1/chat/completions", `{"stream":true,"messages":[{"role":"user","content":"hi"}]}`, tok)
		mockPS5Expect(t, code, 200, body)
		if !strings.HasSuffix(string(body), "data: [DONE]\n\n") {
			t.Fatalf("stream does not end with [DONE]: %s", body)
		}
		code, _ = mockPS5Call(t, "POST", m.Inference.URL+"/v1/chat/completions", chatBody, "wrong")
		mockPS5Expect(t, code, 401, nil)
	})

	t.Run("control", func(t *testing.T) {
		u := m.Control.URL + "/v1/nexus/"
		code, body := mockPS5Call(t, "GET", u+"status", "", tok)
		mockPS5Expect(t, code, 200, body)
		code, _ = mockPS5Call(t, "GET", u+"status", "", "wrong")
		mockPS5Expect(t, code, 401, nil)
		code, body = mockPS5Call(t, "GET", u+"models", "", tok)
		mockPS5Expect(t, code, 200, body)
		code, body = mockPS5Call(t, "POST", u+"models/0123456789abcdef/load", "", tok)
		mockPS5Expect(t, code, 202, body)
		code, body = mockPS5Call(t, "POST", u+"models/fedcba9876543210/load", "", tok)
		mockPS5Expect(t, code, 404, body)
		code, body = mockPS5Call(t, "POST", u+"models/unload", "", tok)
		mockPS5Expect(t, code, 202, body)
		code, body = mockPS5Call(t, "POST", u+"engine/media", "", tok)
		mockPS5Expect(t, code, 202, body)
		_, body = mockPS5Call(t, "GET", u+"status", "", tok)
		if !strings.Contains(string(body), `"engine":"media"`) {
			t.Fatalf("engine not switched: %s", body)
		}
		code, body = mockPS5Call(t, "POST", u+"engine/llm", "", tok)
		mockPS5Expect(t, code, 202, body)
	})

	t.Run("media", func(t *testing.T) {
		u := m.Media.URL
		code, body := mockPS5Call(t, "GET", u+"/health", "", "")
		mockPS5Expect(t, code, 200, body)

		code, body = mockPS5Call(t, "POST", u+"/v1/images/generations", `{"prompt":"a cat"}`, tok)
		mockPS5Expect(t, code, 200, body)
		var img struct {
			Data []struct {
				B64JSON string `json:"b64_json"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &img); err != nil || len(img.Data) != 1 {
			t.Fatalf("image response: %v %s", err, body)
		}
		if _, err := png.DecodeConfig(mustDecodeB64(t, img.Data[0].B64JSON)); err != nil {
			t.Fatalf("image is not a PNG: %v", err)
		}

		code, body = mockPS5Call(t, "POST", u+"/v1/videos/generations", `{"prompt":"a cat"}`, tok)
		mockPS5Expect(t, code, 200, body)
		var vid struct {
			Frames int `json:"frames"`
			Data   []struct {
				B64PNG string `json:"b64_png"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &vid); err != nil || len(vid.Data) != vid.Frames || vid.Frames == 0 {
			t.Fatalf("video response: %v frames=%d data=%d", err, vid.Frames, len(vid.Data))
		}
		for _, f := range vid.Data {
			if _, err := png.DecodeConfig(mustDecodeB64(t, f.B64PNG)); err != nil {
				t.Fatalf("video frame is not a PNG: %v", err)
			}
		}

		code, body = mockPS5Call(t, "POST", u+"/v1/audio/transcriptions", audioBody, tok)
		mockPS5Expect(t, code, 200, body)
		code, body = mockPS5Call(t, "POST", u+"/v1/audio/transcriptions", `{"audio_b64":"bm90IGF1ZGlv"}`, tok)
		mockPS5Expect(t, code, 422, body)

		code, body = mockPS5Call(t, "POST", u+"/v1/audio/speech", `{"input":"hello"}`, tok)
		mockPS5Expect(t, code, 200, body)
		code, body = mockPS5Call(t, "POST", u+"/v1/audio/speech", `{"input":"hello","response_format":"wav"}`, tok)
		mockPS5Expect(t, code, 200, body)
		if !mockPS5IsWAV(body) {
			t.Fatal("speech wav body lacks RIFF/WAVE header")
		}

		code, body = mockPS5Call(t, "POST", u+"/v1/voice/chat", audioBody, tok)
		mockPS5Expect(t, code, 200, body)
		if !strings.Contains(string(body), `"type":"done"`) || !strings.HasSuffix(string(body), "data: [DONE]\n\n") {
			t.Fatalf("voice stream incomplete: %s", body)
		}

		code, _ = mockPS5Call(t, "POST", u+"/v1/images/generations", `{"prompt":"a cat"}`, "wrong")
		mockPS5Expect(t, code, 401, nil)
	})

	t.Run("switch offline", func(t *testing.T) {
		m.Set(func(s *mockPS5Switches) { s.Offline = true })
		defer m.Set(func(s *mockPS5Switches) { *s = mockPS5Switches{} })
		code, _ := mockPS5Call(t, "POST", m.Inference.URL+"/v1/chat/completions", chatBody, tok)
		mockPS5Expect(t, code, 503, nil)
		code, body := mockPS5Call(t, "GET", m.Media.URL+"/health", "", "")
		mockPS5Expect(t, code, 503, body)
		if !strings.Contains(string(body), `"unavailable"`) {
			t.Fatalf("health body: %s", body)
		}
		code, _ = mockPS5Call(t, "GET", m.Control.URL+"/v1/nexus/status", "", tok)
		mockPS5Expect(t, code, 503, nil)
	})

	t.Run("switch busy", func(t *testing.T) {
		m.Set(func(s *mockPS5Switches) { s.Busy = true })
		defer m.Set(func(s *mockPS5Switches) { *s = mockPS5Switches{} })
		code, _ := mockPS5Call(t, "POST", m.Media.URL+"/v1/images/generations", `{"prompt":"a cat"}`, tok)
		mockPS5Expect(t, code, 409, nil)
		code, _ = mockPS5Call(t, "POST", m.Control.URL+"/v1/nexus/models/0123456789abcdef/load", "", tok)
		mockPS5Expect(t, code, 409, nil)
		code, _ = mockPS5Call(t, "GET", m.Control.URL+"/v1/nexus/status", "", tok)
		mockPS5Expect(t, code, 200, nil)
	})

	t.Run("switch slow", func(t *testing.T) {
		m.Set(func(s *mockPS5Switches) { s.Slow = 50 * time.Millisecond })
		defer m.Set(func(s *mockPS5Switches) { *s = mockPS5Switches{} })
		start := time.Now()
		code, body := mockPS5Call(t, "GET", m.Media.URL+"/health", "", "")
		mockPS5Expect(t, code, 200, body)
		if time.Since(start) < 50*time.Millisecond {
			t.Fatal("slow switch did not delay the response")
		}
	})

	t.Run("switch error event", func(t *testing.T) {
		m.Set(func(s *mockPS5Switches) { s.ErrorEvent = true })
		defer m.Set(func(s *mockPS5Switches) { *s = mockPS5Switches{} })
		code, body := mockPS5Call(t, "POST", m.Inference.URL+"/v1/chat/completions", `{"stream":true,"messages":[{"role":"user","content":"hi"}]}`, tok)
		mockPS5Expect(t, code, 200, body)
		if !strings.Contains(string(body), `"http_status":500`) || !strings.HasSuffix(string(body), "data: [DONE]\n\n") {
			t.Fatalf("stream lacks error event: %s", body)
		}
	})
}

func mustDecodeB64(t *testing.T, s string) *bytes.Reader {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("base64: %v", err)
	}
	return bytes.NewReader(raw)
}
