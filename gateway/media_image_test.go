package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// mediaImageStub serves the PS5 media API from mock. Image generation is counted, and answered by images when it is set.
func mediaImageStub(t *testing.T, mock *mockPS5, images http.HandlerFunc) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	routes := mock.mediaRoutes()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/images/generations" {
			calls.Add(1)
			if images != nil {
				images(w, r)
				return
			}
		}
		routes.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// mediaImageTestGateway wires a gateway to a PS5 stand-in whose image route is answered by images.
func mediaImageTestGateway(t *testing.T, images http.HandlerFunc) (*Gateway, *atomic.Int32) {
	t.Helper()
	mediaRunnerSetTimers(t, 2*time.Millisecond, mediaSwitchTimeout)
	mock := newMockPS5(t)
	inference := httptest.NewServer(mock.inferenceRoutes())
	t.Cleanup(inference.Close)
	media, calls := mediaImageStub(t, mock, images)
	g := mediaRunnerGateway(t, mock.Control.URL, media.URL, inference.URL, 10)
	// Admission reads the last health snapshot, so take one before the request.
	g.ps5.health.Poll(context.Background())
	return g, calls
}

func mediaImageTestRequest(body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(body))
	return r.WithContext(context.WithValue(r.Context(), principalKey{}, "alice"))
}

func TestImageJSONPathReturnsPNG(t *testing.T) {
	g, calls := mediaImageTestGateway(t, nil)
	rec := httptest.NewRecorder()
	g.handleImage()(rec, mediaImageTestRequest(`{"prompt":"a red fox","width":256,"height":256,"steps":4,"seed":42}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var out struct {
		Data []struct {
			B64JSON string `json:"b64_json"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || len(out.Data) != 1 {
		t.Fatalf("reply shape wrong: %v", err)
	}
	img, err := base64.StdEncoding.DecodeString(out.Data[0].B64JSON)
	if err != nil || !mediaValIsPNG(img) {
		t.Fatal("reply is not a base64 PNG")
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("PS5 image calls = %d, want 1", n)
	}
}

func TestImageStreamForwardsProgressThenFinal(t *testing.T) {
	png := mockPS5PNG(8, 8)
	images := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"status\":\"loading\"}\n\n")
		fmt.Fprint(w, "data: {\"status\":\"generating\",\"step\":1,\"steps\":2}\n\n")
		fmt.Fprint(w, "data: {\"created\":1,\"data\":[{\"b64_json\":\""+png+"\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}
	g, _ := mediaImageTestGateway(t, images)
	rec := httptest.NewRecorder()
	g.handleImage()(rec, mediaImageTestRequest(`{"prompt":"a red fox","width":256,"height":256,"stream":true}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type = %q, want text/event-stream", ct)
	}
	var events []string
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if payload, ok := strings.CutPrefix(line, "data: "); ok {
			events = append(events, payload)
		}
	}
	if len(events) != 4 {
		t.Fatalf("events = %d, want 4 (loading, generating, final, [DONE])", len(events))
	}
	for i, want := range []string{"loading", "generating", "b64_json", "[DONE]"} {
		if !strings.Contains(events[i], want) {
			t.Fatalf("event %d = %q, want it to contain %q", i, events[i], want)
		}
	}
}

func TestImageRejectsBeforeQueueing(t *testing.T) {
	cases := map[string]string{
		"width not a multiple of 64": `{"prompt":"x","width":300,"height":256}`,
		"height above 1024":          `{"prompt":"x","width":256,"height":2048}`,
		"model not allow-listed":     `{"prompt":"x","width":256,"height":256,"model":"not-listed"}`,
		"model path traversal":       `{"prompt":"x","width":256,"height":256,"model":"../sd-turbo-fp16"}`,
		"steps out of range":         `{"prompt":"x","width":256,"height":256,"steps":0}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			g, calls := mediaImageTestGateway(t, nil)
			rec := httptest.NewRecorder()
			g.handleImage()(rec, mediaImageTestRequest(body))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
			if n := calls.Load(); n != 0 {
				t.Fatalf("PS5 image calls = %d, want 0", n)
			}
			g.queue.mu.Lock()
			queued := len(g.queue.queued)
			g.queue.mu.Unlock()
			if queued != 0 {
				t.Fatalf("queued jobs = %d, want 0", queued)
			}
		})
	}
}

func TestImageNonPNGUpstreamIs502(t *testing.T) {
	notPNG := base64.StdEncoding.EncodeToString([]byte("GIF89a not a png"))
	images := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"data":[{"b64_json":"%s"}]}`, notPNG)
	}
	g, _ := mediaImageTestGateway(t, images)
	rec := httptest.NewRecorder()
	g.handleImage()(rec, mediaImageTestRequest(`{"prompt":"x","width":256,"height":256}`))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
}
