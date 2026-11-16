package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const ps5ClientTestToken = "test-token-0123456789abcdef0123456789abcdef"

// ps5ClientTestServer returns a PS5 client whose control and media base URLs both point at handler.
// The handler must present the expected bearer, otherwise the test fails.
func ps5ClientTestServer(t *testing.T, handler http.HandlerFunc) *ps5Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+ps5ClientTestToken {
			t.Errorf("unexpected Authorization header on %s", r.URL.Path)
		}
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	c, err := newPS5Client(srv.URL, srv.URL, func() (string, error) { return ps5ClientTestToken, nil })
	if err != nil {
		t.Fatalf("newPS5Client: %v", err)
	}
	return c
}

func ps5ClientMustNotContainToken(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), ps5ClientTestToken) {
		t.Fatalf("bearer leaked into error string: %q", err.Error())
	}
}

func TestPS5ClientMapsErrorShapes(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		wantSt  int
		wantTyp string
		wantMsg string
	}{
		{"auth nested", 401, `{"error":{"message":"valid Bearer API key required","type":"authentication_error"}}`, 401, "authentication_error", "valid Bearer API key required"},
		{"model load", 503, `{"error":{"message":"no model loaded","type":"model_load_error"}}`, 503, "model_load_error", "no model loaded"},
		{"invalid request", 422, `{"error":{"message":"max_tokens out of range","type":"invalid_request_error"}}`, 422, "invalid_request_error", "max_tokens out of range"},
		{"server error", 500, `{"error":{"message":"out of memory","type":"server_error"}}`, 500, "server_error", "out of memory"},
		{"voice flat shape", 400, `{"type":"error","message":"bad audio"}`, 400, "", "bad audio"},
		{"plain text fallback", 404, "not found", 404, "", "PS5 returned Not Found"},
		{"string error fallback", 409, `{"error":"busy"}`, 409, "", "PS5 returned Conflict"},
		{"unknown type dropped", 413, `{"error":{"message":"body too large","type":"custom_\u0000type"}}`, 413, "", "body too large"},
		{"control characters stripped", 422, "{\"error\":{\"message\":\"bad\\u0007\\nvalue\",\"type\":\"invalid_request_error\"}}", 422, "invalid_request_error", "bad  value"},
		{"upstream echoes bearer", 500, `{"error":{"message":"rejected ` + ps5ClientTestToken + `","type":"server_error"}}`, 500, "server_error", "PS5 returned Internal Server Error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := ps5ClientTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			})
			err := c.postJSON(context.Background(), c.media, "/v1/chat/completions", ps5ChatTimeout, map[string]string{"x": "y"}, nil)
			ps5ClientMustNotContainToken(t, err)
			pe, ok := err.(*ps5Error)
			if !ok {
				t.Fatalf("error type = %T, want *ps5Error", err)
			}
			if pe.Status != tc.wantSt || pe.Type != tc.wantTyp || pe.Message != tc.wantMsg {
				t.Fatalf("got status=%d type=%q msg=%q; want status=%d type=%q msg=%q",
					pe.Status, pe.Type, pe.Message, tc.wantSt, tc.wantTyp, tc.wantMsg)
			}
		})
	}
}

func TestPS5ClientBearerNeverInErrors(t *testing.T) {
	t.Run("bearer func fails with secret detail", func(t *testing.T) {
		c, err := newPS5Client("http://127.0.0.1:1", "http://127.0.0.1:1", func() (string, error) {
			return "", errors.New("cannot read C:/creds/" + ps5ClientTestToken)
		})
		if err != nil {
			t.Fatal(err)
		}
		err = c.postJSON(context.Background(), c.control, "/v1/nexus/engine/media", ps5EngineSwitchTimeout, nil, nil)
		ps5ClientMustNotContainToken(t, err)
		if strings.Contains(err.Error(), "C:/creds") {
			t.Fatalf("bearer loader detail leaked: %q", err.Error())
		}
	})
	t.Run("unreachable host", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		addr := srv.URL
		srv.Close()
		c, err := newPS5Client(addr, addr, func() (string, error) { return ps5ClientTestToken, nil })
		if err != nil {
			t.Fatal(err)
		}
		err = c.postJSON(context.Background(), c.media, "/v1/images/generations", ps5ImageTimeout, map[string]string{}, nil)
		ps5ClientMustNotContainToken(t, err)
		if !strings.Contains(err.Error(), "PS5 unreachable") {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestPS5ClientRefusesRedirect(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	c := ps5ClientTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/steal", http.StatusTemporaryRedirect)
	})
	err := c.postJSON(context.Background(), c.media, "/v1/voice/chat", ps5VoiceTimeout, map[string]string{}, nil)
	ps5ClientMustNotContainToken(t, err)
	pe, ok := err.(*ps5Error)
	if !ok || pe.Message != "PS5 redirect refused" {
		t.Fatalf("got %v, want redirect refused", err)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("redirect target was contacted %d times", n)
	}
}

func TestPS5ClientSSEForwardsEventsInOrder(t *testing.T) {
	c := ps5ClientTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":1,\"delta\":\"a\"}\n\n: keep-alive\n\ndata: {\"id\":2,\"delta\":\"b\"}\n\ndata: {\"id\":3}\n\ndata: [DONE]\n\n")
	})
	var got []string
	err := c.postSSE(context.Background(), c.media, "/v1/chat/completions", ps5ChatTimeout, map[string]bool{"stream": true}, func(data []byte) error {
		got = append(got, string(data))
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{`{"id":1,"delta":"a"}`, `{"id":2,"delta":"b"}`, `{"id":3}`}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("events = %q, want %q", got, want)
	}
}

func TestPS5ClientSSEDetectsErrorEvent(t *testing.T) {
	cases := []struct {
		name    string
		stream  string
		wantSt  int
		wantTyp string
		wantMsg string
		nEvents int
	}{
		{
			name:    "streamed error with http_status",
			stream:  "data: {\"id\":1,\"delta\":\"a\"}\n\ndata: {\"error\":{\"message\":\"context overflow\",\"type\":\"invalid_request_error\"},\"http_status\":422}\n\ndata: [DONE]\n\n",
			wantSt:  422,
			wantTyp: "invalid_request_error",
			wantMsg: "context overflow",
			nEvents: 2,
		},
		{
			name:    "voice error event without http_status",
			stream:  "data: {\"type\":\"transcript\",\"text\":\"hi\"}\n\ndata: {\"type\":\"error\",\"message\":\"tts failed\"}\n\ndata: [DONE]\n\n",
			wantMsg: "tts failed",
			nEvents: 2,
		},
		{
			name:    "stream without DONE",
			stream:  "data: {\"id\":1}\n\n",
			wantMsg: "PS5 stream ended early",
			nEvents: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := ps5ClientTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, tc.stream)
			})
			var n int
			err := c.postSSE(context.Background(), c.media, "/v1/voice/chat", ps5VoiceTimeout, map[string]string{}, func([]byte) error {
				n++
				return nil
			})
			ps5ClientMustNotContainToken(t, err)
			pe, ok := err.(*ps5Error)
			if !ok {
				t.Fatalf("error type = %T (%v), want *ps5Error", err, err)
			}
			if pe.Status != tc.wantSt || pe.Type != tc.wantTyp || pe.Message != tc.wantMsg {
				t.Fatalf("got status=%d type=%q msg=%q", pe.Status, pe.Type, pe.Message)
			}
			if n != tc.nEvents {
				t.Fatalf("forwarded %d events, want %d", n, tc.nEvents)
			}
		})
	}
}

func TestPS5ClientTimeoutAppliesPerEndpoint(t *testing.T) {
	c := ps5ClientTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body) // lets the server notice the client hanging up
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	})
	err := c.postJSON(context.Background(), c.media, "/v1/audio/speech", 50*time.Millisecond, map[string]string{}, nil)
	ps5ClientMustNotContainToken(t, err)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error %v does not wrap context.DeadlineExceeded", err)
	}
}

func TestPS5ClientDecodesSuccessAndRejectsBadBaseURLs(t *testing.T) {
	c := ps5ClientTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "switching", "restart_required": true})
	})
	var out struct {
		Status          string `json:"status"`
		RestartRequired bool   `json:"restart_required"`
	}
	if err := c.postJSON(context.Background(), c.control, "/v1/nexus/engine/llm", ps5EngineSwitchTimeout, nil, &out); err != nil {
		t.Fatalf("202 response returned error: %v", err)
	}
	if out.Status != "switching" || !out.RestartRequired {
		t.Fatalf("decoded %+v", out)
	}
	if err := c.switchEngine(context.Background(), "video"); err == nil {
		t.Fatal("unknown engine accepted")
	}
	for _, bad := range []string{"", "ftp://127.0.0.1:8082", "http://user:pass@127.0.0.1:8082", "/relative"} {
		if _, err := newPS5Client(bad, "http://127.0.0.1:8083", func() (string, error) { return "", nil }); err == nil {
			t.Fatalf("base URL %q accepted", bad)
		}
	}
	if _, err := newPS5Client("http://127.0.0.1:8082", "http://127.0.0.1:8083", nil); err == nil {
		t.Fatal("nil bearer accepted")
	}
}
