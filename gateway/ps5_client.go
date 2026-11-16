package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
)

// Per-endpoint request timeouts for PSllama (plan §2.2 U-07). The chat value is the
// NEXUS_AI_REQUEST_TIMEOUT default; the caller decides whether to pass an override.
const (
	ps5ChatTimeout         = 15 * time.Minute
	ps5ImageTimeout        = 10 * time.Minute
	ps5VideoTimeout        = 60 * time.Minute
	ps5STTTimeout          = 2 * time.Minute
	ps5TTSTimeout          = 5 * time.Minute
	ps5VoiceTimeout        = 3 * time.Minute
	ps5EngineSwitchTimeout = 10 * time.Minute
)

const (
	ps5MaxErrorBodyBytes    = 64 << 10
	ps5MaxResponseBytes     = 256 << 20
	ps5MaxSSELineBytes      = 16 << 20 // voice and speech events carry base64 audio on one line
	ps5MaxErrorMessageRunes = 200
)

// errPS5RedirectRefused is returned by CheckRedirect; the bearer is never sent to a redirect target.
var errPS5RedirectRefused = errors.New("ps5 redirect refused")

// ps5Error is the sanitized failure type returned by every PS5 call. Message never contains the
// bearer, request bodies, prompts, or transport details.
type ps5Error struct {
	Status  int    // HTTP status from PSllama (or its http_status field); 0 for transport or stream failures
	Type    string // PSllama error type when it is one of the known values, else ""
	Message string // safe to show to users
	cause   error  // context.DeadlineExceeded or context.Canceled, for errors.Is
}

func (e *ps5Error) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("ps5: HTTP %d: %s", e.Status, e.Message)
	}
	return "ps5: " + e.Message
}

func (e *ps5Error) Unwrap() error { return e.cause }

// ps5Client talks to the PS5 control API and media API at fixed base URLs. The bearer is loaded
// per request through bearer, so rotated credentials are picked up without a restart.
type ps5Client struct {
	control *url.URL
	media   *url.URL
	bearer  func() (string, error)
	http    *http.Client
}

func newPS5Client(controlBase, mediaBase string, bearer func() (string, error)) (*ps5Client, error) {
	if bearer == nil {
		return nil, errors.New("ps5 client: bearer func is nil")
	}
	control, err := ps5ParseBase(controlBase)
	if err != nil {
		return nil, fmt.Errorf("ps5 control base URL %w", err)
	}
	media, err := ps5ParseBase(mediaBase)
	if err != nil {
		return nil, fmt.Errorf("ps5 media base URL %w", err)
	}
	return &ps5Client{
		control: control,
		media:   media,
		bearer:  bearer,
		http: &http.Client{
			// No Proxy: environment proxies must not receive the bearer.
			Transport: &http.Transport{DialContext: (&net.Dialer{Timeout: 10 * time.Second}).DialContext},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errPS5RedirectRefused
			},
		},
	}, nil
}

func ps5ParseBase(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return nil, errors.New("must be an absolute http(s) URL without credentials")
	}
	return u, nil
}

// post sends an authenticated POST with a per-endpoint timeout. Non-2xx responses become *ps5Error.
// Successful responses are passed to handle, which must consume the body before returning.
func (c *ps5Client) post(ctx context.Context, base *url.URL, path string, timeout time.Duration, in any, handle func(resp *http.Response, secret string) error) error {
	secret, err := c.bearer()
	if err != nil || secret == "" {
		return &ps5Error{Message: "PS5 credential unavailable"}
	}
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return &ps5Error{Message: "invalid PS5 request"}
		}
		body = bytes.NewReader(raw)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base.JoinPath(path).String(), body)
	if err != nil {
		return &ps5Error{Message: "invalid PS5 request"}
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return ps5TransportError(ctx, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, ps5MaxErrorBodyBytes))
		return ps5ErrorFromBody(resp.StatusCode, raw, secret)
	}
	return handle(resp, secret)
}

// postJSON decodes a 2xx JSON body into out. out may be nil to discard the body.
func (c *ps5Client) postJSON(ctx context.Context, base *url.URL, path string, timeout time.Duration, in, out any) error {
	return c.post(ctx, base, path, timeout, in, func(resp *http.Response, _ string) error {
		body := io.LimitReader(resp.Body, ps5MaxResponseBytes)
		if out == nil {
			_, _ = io.Copy(io.Discard, body)
			return nil
		}
		if err := json.NewDecoder(body).Decode(out); err != nil {
			return &ps5Error{Status: resp.StatusCode, Message: "PS5 returned an invalid response"}
		}
		return nil
	})
}

// postSSE forwards each SSE data payload to onEvent in order. See ps5ScanSSE for error handling.
func (c *ps5Client) postSSE(ctx context.Context, base *url.URL, path string, timeout time.Duration, in any, onEvent func(data []byte) error) error {
	return c.post(ctx, base, path, timeout, in, func(resp *http.Response, secret string) error {
		return ps5ScanSSE(resp.Body, secret, onEvent)
	})
}

// switchEngine asks the control API to move PSllama to "media" or "llm" mode. A 202 means the PS5 is
// restarting into the new engine; a 409 means it is busy or already switching.
func (c *ps5Client) switchEngine(ctx context.Context, engine string) error {
	if engine != "media" && engine != "llm" {
		return &ps5Error{Message: "unknown engine"}
	}
	return c.postJSON(ctx, c.control, "/v1/nexus/engine/"+engine, ps5EngineSwitchTimeout, nil, nil)
}

// ps5ScanSSE forwards each data payload (except [DONE]) to onEvent. A PSllama error event is
// forwarded and then ends the stream with a *ps5Error. A stream that ends without [DONE] is an error,
// because PSllama always terminates its streams with it.
func ps5ScanSSE(r io.Reader, secret string, onEvent func(data []byte) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), ps5MaxSSELineBytes)
	var data []string
	sawDone := false
	// dispatch returns stop=true when the stream is finished (after [DONE] or an error event).
	dispatch := func() (stop bool, err error) {
		if len(data) == 0 {
			return false, nil
		}
		payload := strings.Join(data, "\n")
		data = data[:0]
		if payload == "[DONE]" {
			sawDone = true
			return true, nil
		}
		if err := onEvent([]byte(payload)); err != nil {
			return true, err
		}
		var probe struct {
			Error json.RawMessage `json:"error"`
			Type  string          `json:"type"`
		}
		_ = json.Unmarshal([]byte(payload), &probe)
		isErr := (len(probe.Error) > 0 && string(probe.Error) != "null") || probe.Type == "error"
		if isErr {
			return true, ps5ErrorFromBody(0, []byte(payload), secret)
		}
		return false, nil
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if stop, err := dispatch(); stop {
				return err
			}
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if sc.Err() != nil {
		return &ps5Error{Message: "PS5 stream interrupted"}
	}
	if _, err := dispatch(); err != nil {
		return err
	}
	if !sawDone {
		return &ps5Error{Message: "PS5 stream ended early"}
	}
	return nil
}

func ps5TransportError(ctx context.Context, err error) *ps5Error {
	switch {
	case errors.Is(err, errPS5RedirectRefused):
		return &ps5Error{Message: "PS5 redirect refused"}
	case ctx.Err() != nil:
		msg := "PS5 request canceled"
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			msg = "PS5 request timed out"
		}
		return &ps5Error{Message: msg, cause: ctx.Err()}
	}
	return &ps5Error{Message: "PS5 unreachable"}
}

// ps5ErrorFromBody maps a PSllama error body to a *ps5Error. It accepts the nested
// {"error":{"message","type"}} shape and the flat {"type":"error","message"} voice shape, plus an
// http_status field on streamed errors. status 0 means "take http_status from the body".
func ps5ErrorFromBody(status int, raw []byte, secret string) *ps5Error {
	var env struct {
		Error *struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
		Type       string `json:"type"`
		Message    string `json:"message"`
		HTTPStatus int    `json:"http_status"`
	}
	_ = json.Unmarshal(raw, &env) // non-JSON or unexpected shapes leave env zero
	msg, typ := env.Message, env.Type
	if env.Error != nil {
		msg, typ = env.Error.Message, env.Error.Type
	}
	if status == 0 {
		status = env.HTTPStatus
	}
	e := &ps5Error{Status: status, Type: ps5KnownType(typ), Message: ps5SafeMessage(msg, secret)}
	if e.Message == "" {
		e.Message = ps5StatusMessage(status)
	}
	return e
}

func ps5KnownType(t string) string {
	switch t {
	case "invalid_request_error", "authentication_error", "server_error", "model_load_error":
		return t
	}
	return ""
}

// ps5SafeMessage drops upstream text that echoes the bearer, strips control characters, and caps the length.
func ps5SafeMessage(msg, secret string) string {
	if secret != "" && strings.Contains(msg, secret) {
		return ""
	}
	msg = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, msg))
	if runes := []rune(msg); len(runes) > ps5MaxErrorMessageRunes {
		msg = string(runes[:ps5MaxErrorMessageRunes])
	}
	return msg
}

func ps5StatusMessage(status int) string {
	if s := http.StatusText(status); s != "" {
		return "PS5 returned " + s
	}
	return "PS5 request failed"
}
