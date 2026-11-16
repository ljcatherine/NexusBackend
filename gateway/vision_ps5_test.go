package main

// Vision through the PS5 chat path (plan U-18, decision D-6). Tests only: the image rules they check are
// implemented in main.go by U-24. Set visionD6Implemented to true once U-24 lands to enable the D-6 cases.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// visionD6Implemented turns on the D-6 cases (PNG and JPEG up to 4 MiB, magic-byte checked).
const visionD6Implemented = true

const visionImageMiB = 1 << 20

// visionCapture is the fake PS5 inference server: it records each chat body it receives and answers chat
// requests with a fixed status and body. Every other route goes to the mock's own inference routes.
type visionCapture struct {
	mu       sync.Mutex
	bodies   [][]byte
	status   int    // 0 means relay to the mock
	response string // body used when status is set
}

func (c *visionCapture) chatBodies() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][]byte(nil), c.bodies...)
}

func visionPS5Server(t *testing.T, mock *mockPS5, capture *visionCapture) *httptest.Server {
	t.Helper()
	routes := mock.inferenceRoutes()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			routes.ServeHTTP(w, r)
			return
		}
		var raw bytes.Buffer
		_, _ = raw.ReadFrom(r.Body)
		capture.mu.Lock()
		capture.bodies = append(capture.bodies, raw.Bytes())
		status, response := capture.status, capture.response
		capture.mu.Unlock()
		if status != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(response))
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(raw.Bytes()))
		routes.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	return server
}

// visionRequest builds a chat request whose user message is text plus one image_url part.
func visionRequest(t *testing.T, token, imageURL string) *http.Request {
	t.Helper()
	content := []map[string]any{
		{"type": "text", "text": "what is in this picture"},
		{"type": "image_url", "image_url": map[string]string{"url": imageURL}},
	}
	body, err := json.Marshal(map[string]any{
		"model":    "model-installed-by-owner",
		"stream":   false,
		"messages": []map[string]any{{"role": "user", "content": content}},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	return request
}

// visionForwardedURLs returns the image_url values the PS5 received, in order.
func visionForwardedURLs(t *testing.T, bodies [][]byte) []string {
	t.Helper()
	var urls []string
	for _, raw := range bodies {
		var req struct {
			Messages []struct {
				Content []struct {
					Type     string `json:"type"`
					ImageURL *struct {
						URL string `json:"url"`
					} `json:"image_url"`
				} `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			t.Fatalf("PS5 received an undecodable chat body: %v", err)
		}
		for _, message := range req.Messages {
			for _, part := range message.Content {
				if part.ImageURL != nil {
					urls = append(urls, part.ImageURL.URL)
				}
			}
		}
	}
	return urls
}

func visionJPEGDataURL(t *testing.T, w, h int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 16), G: uint8(y * 16), B: 0x40, A: 0xff})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatal(err)
	}
	return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}

// visionSizedDataURL returns a data URL whose decoded payload is n bytes, with the given magic bytes and
// the JPEG end-of-image marker so only the size rule decides.
func visionSizedDataURL(mime string, magic []byte, n int) string {
	payload := make([]byte, n)
	copy(payload, magic)
	if n >= 2 {
		payload[n-2], payload[n-1] = 0xFF, 0xD9
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(payload)
}

var (
	visionJPEGMagic = []byte{0xFF, 0xD8, 0xFF, 0xE0}
	visionPNGMagic  = []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}
)

func TestVisionPS5JPEGReachesPS5Unchanged(t *testing.T) {
	mock := newMockPS5(t)
	capture := &visionCapture{}
	inference := visionPS5Server(t, mock, capture)
	gateway := testPS5Gateway(t, mock, inference)
	if state := gateway.ps5.health.Poll(t.Context()); !state.Online || state.Engine != "llm" {
		t.Fatalf("PS5 not online for vision chat: %+v", state)
	}

	dataURL := visionJPEGDataURL(t, 32, 24)
	response := httptest.NewRecorder()
	gateway.Handler().ServeHTTP(response, visionRequest(t, "alice-token", dataURL))
	if response.Code != http.StatusOK {
		t.Fatalf("JPEG chat status = %d, want 200: %s", response.Code, response.Body.String())
	}
	bodies := capture.chatBodies()
	if len(bodies) != 1 {
		t.Fatalf("PS5 received %d chat requests, want 1", len(bodies))
	}
	urls := visionForwardedURLs(t, bodies)
	if len(urls) != 1 || urls[0] != dataURL {
		t.Fatalf("image_url forwarded to PS5 differs from the request (got %d URLs)", len(urls))
	}
}

func TestVisionPS5PNGAccepted(t *testing.T) {
	if !visionD6Implemented {
		t.Skip("needs U-24 (D-6)")
	}
	mock := newMockPS5(t)
	capture := &visionCapture{}
	inference := visionPS5Server(t, mock, capture)
	gateway := testPS5Gateway(t, mock, inference)
	if state := gateway.ps5.health.Poll(t.Context()); !state.Online {
		t.Fatalf("PS5 not online: %+v", state)
	}

	dataURL := "data:image/png;base64," + mockPS5PNG(16, 16)
	response := httptest.NewRecorder()
	gateway.Handler().ServeHTTP(response, visionRequest(t, "alice-token", dataURL))
	if response.Code != http.StatusOK {
		t.Fatalf("PNG chat status = %d, want 200: %s", response.Code, response.Body.String())
	}
	urls := visionForwardedURLs(t, capture.chatBodies())
	if len(urls) != 1 || urls[0] != dataURL {
		t.Fatalf("PNG image_url not forwarded unchanged (got %d URLs)", len(urls))
	}
}

// D-6 boundary: a single image of exactly 4 MiB is accepted. This also needs U-24 to raise the combined
// image cap (currently 1 MiB) and the encoded-length cap (currently 700 KiB); the test will catch either gap.
func TestVisionPS5ImageAtFourMiBAccepted(t *testing.T) {
	if !visionD6Implemented {
		t.Skip("needs U-24 (D-6)")
	}
	mock := newMockPS5(t)
	capture := &visionCapture{}
	inference := visionPS5Server(t, mock, capture)
	gateway := testPS5Gateway(t, mock, inference)
	if state := gateway.ps5.health.Poll(t.Context()); !state.Online {
		t.Fatalf("PS5 not online: %+v", state)
	}

	dataURL := visionSizedDataURL("image/jpeg", visionJPEGMagic, 4*visionImageMiB)
	response := httptest.NewRecorder()
	gateway.Handler().ServeHTTP(response, visionRequest(t, "alice-token", dataURL))
	if response.Code != http.StatusOK {
		t.Fatalf("4 MiB JPEG status = %d, want 200", response.Code)
	}
}

func TestVisionPS5ImageOverFourMiBRejected(t *testing.T) {
	if !visionD6Implemented {
		t.Skip("needs U-24 (D-6)")
	}
	mock := newMockPS5(t)
	capture := &visionCapture{}
	inference := visionPS5Server(t, mock, capture)
	gateway := testPS5Gateway(t, mock, inference)
	if state := gateway.ps5.health.Poll(t.Context()); !state.Online {
		t.Fatalf("PS5 not online: %+v", state)
	}

	for name, dataURL := range map[string]string{
		"jpeg": visionSizedDataURL("image/jpeg", visionJPEGMagic, 4*visionImageMiB+1),
		"png":  visionSizedDataURL("image/png", visionPNGMagic, 4*visionImageMiB+1),
	} {
		response := httptest.NewRecorder()
		gateway.Handler().ServeHTTP(response, visionRequest(t, "alice-token", dataURL))
		if response.Code != http.StatusRequestEntityTooLarge && response.Code != http.StatusBadRequest {
			t.Fatalf("%s 4 MiB+1 status = %d, want 413 or 400", name, response.Code)
		}
	}
	if n := len(capture.chatBodies()); n != 0 {
		t.Fatalf("oversized images reached the PS5 (%d chat requests)", n)
	}
}

// Runs now: the current rule (JPEG only) already rejects GIF and text, and D-6 keeps that for non-images.
func TestVisionPS5NonImageDataURLRejected(t *testing.T) {
	mock := newMockPS5(t)
	capture := &visionCapture{}
	inference := visionPS5Server(t, mock, capture)
	gateway := testPS5Gateway(t, mock, inference)
	if state := gateway.ps5.health.Poll(t.Context()); !state.Online {
		t.Fatalf("PS5 not online: %+v", state)
	}

	gif := base64.StdEncoding.EncodeToString([]byte("GIF89a\x01\x00\x01\x00\x00\x00\x00;"))
	text := base64.StdEncoding.EncodeToString([]byte("not an image, just text"))
	for name, dataURL := range map[string]string{
		"gif":        "data:image/gif;base64," + gif,
		"text":       "data:text/plain;base64," + text,
		"svg":        "data:image/svg+xml;base64," + text,
		"http link":  "https://example.invalid/picture.jpg",
		"bad base64": "data:image/jpeg;base64,!!!!",
	} {
		response := httptest.NewRecorder()
		gateway.Handler().ServeHTTP(response, visionRequest(t, "alice-token", dataURL))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s status = %d, want 400: %s", name, response.Code, response.Body.String())
		}
		if strings.Contains(response.Body.String(), text) {
			t.Fatalf("%s rejection echoed the payload", name)
		}
	}
	if n := len(capture.chatBodies()); n != 0 {
		t.Fatalf("non-image attachments reached the PS5 (%d chat requests)", n)
	}
}

// Runs now: a text-only model makes the PS5 refuse an image with an error that may name internal details.
// The gateway must answer with its fixed 502 message and never echo the upstream body.
func TestVisionPS5TextOnlyModelErrorSanitized(t *testing.T) {
	mock := newMockPS5(t)
	const upstreamDetail = "image input is not supported - mmproj at /models/internal/mmproj-canary.gguf"
	capture := &visionCapture{
		status:   http.StatusBadRequest,
		response: `{"error":{"message":"` + upstreamDetail + `","type":"invalid_request_error"}}`,
	}
	inference := visionPS5Server(t, mock, capture)
	gateway := testPS5Gateway(t, mock, inference)
	if state := gateway.ps5.health.Poll(t.Context()); !state.Online {
		t.Fatalf("PS5 not online: %+v", state)
	}

	response := httptest.NewRecorder()
	gateway.Handler().ServeHTTP(response, visionRequest(t, "alice-token", visionJPEGDataURL(t, 16, 16)))
	const want = `{"error":{"message":"AI inference request failed"}}`
	if response.Code != http.StatusBadGateway {
		t.Fatalf("text-only refusal status = %d, want 502: %s", response.Code, response.Body.String())
	}
	if got := strings.TrimSpace(response.Body.String()); got != want {
		t.Fatalf("text-only refusal body = %q, want %q", got, want)
	}
	if strings.Contains(response.Body.String(), "canary") || strings.Contains(response.Body.String(), "mmproj") {
		t.Fatal("upstream error detail leaked to the client")
	}
	if len(capture.chatBodies()) != 1 {
		t.Fatal("image chat did not reach the PS5 once")
	}
}
