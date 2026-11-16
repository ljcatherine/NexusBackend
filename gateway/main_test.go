package main

import (
	"bytes"
	"context"
	"crypto"
	cryptorand "crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

type fakeVerifier struct{ subjects map[string]string }

func (v fakeVerifier) Subject(_ context.Context, token string) (string, error) {
	if subject := v.subjects[token]; subject != "" {
		return subject, nil
	}
	return "", errors.New("invalid token")
}

func signTestAccessToken(t *testing.T, key *rsa.PrivateKey, issuer, audience, subject string, expiry time.Time) string {
	t.Helper()
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "at+JWT", "kid": "test-key"})
	claims, _ := json.Marshal(map[string]any{
		"iss": issuer, "aud": audience, "sub": subject,
		"iat": time.Now().Unix(), "exp": expiry.Unix(),
	})
	unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(cryptorand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func TestOIDCAccessTokenVerifierChecksSignatureIssuerAudienceAndExpiry(t *testing.T) {
	key, err := rsa.GenerateKey(cryptorand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	issuer := "https://identity.example.test/"
	verifier := &OIDCAccessTokenVerifier{verifier: oidc.NewVerifier(issuer,
		&oidc.StaticKeySet{PublicKeys: []crypto.PublicKey{&key.PublicKey}},
		&oidc.Config{ClientID: "https://gateway.example.test"})}
	valid := signTestAccessToken(t, key, issuer, "https://gateway.example.test", "configured-owner-sub", time.Now().Add(time.Minute))
	subject, err := verifier.Subject(context.Background(), valid)
	if err != nil || subject != "configured-owner-sub" {
		t.Fatalf("valid token failed verification: subject=%q err=%v", subject, err)
	}
	wrongIssuer := signTestAccessToken(t, key, "https://attacker.example.test/", "https://gateway.example.test", "configured-owner-sub", time.Now().Add(time.Minute))
	wrongAudience := signTestAccessToken(t, key, issuer, "other-client", "configured-owner-sub", time.Now().Add(time.Minute))
	expired := signTestAccessToken(t, key, issuer, "https://gateway.example.test", "configured-owner-sub", time.Now().Add(-time.Minute))
	otherKey, err := rsa.GenerateKey(cryptorand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	wrongSignature := signTestAccessToken(t, otherKey, issuer, "https://gateway.example.test", "configured-owner-sub", time.Now().Add(time.Minute))
	for name, token := range map[string]string{"issuer": wrongIssuer, "audience": wrongAudience, "expiry": expired, "signature": wrongSignature} {
		t.Run(name, func(t *testing.T) {
			if _, err := verifier.Subject(context.Background(), token); err == nil {
				t.Fatal("invalid token was accepted")
			}
		})
	}
}

func testGateway(upstream *httptest.Server, permissions map[string]map[string]bool) *Gateway {
	parsed, _ := url.Parse(upstream.URL)
	parsed.Path = "/v1"
	return NewGateway(Config{
		SubjectPermissions:  permissions,
		Upstream:            parsed,
		UpstreamBearerToken: "server-only-upstream-secret",
		AllowedModels:       map[string]bool{"model-installed-by-owner": true},
		RequestTimeout:      time.Second,
		RequestsPerMinute:   5,
		MaximumConcurrentAI: 1,
	}, fakeVerifier{subjects: map[string]string{"valid-token": "owner-subject"}})
}

func TestUnauthenticatedRequestDoesNotReachUpstream(t *testing.T) {
	called := false
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer upstream.Close()
	gateway := testGateway(upstream, map[string]map[string]bool{"owner-subject": {permissionModels: true}})
	response := httptest.NewRecorder()
	gateway.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if response.Code != http.StatusUnauthorized || called {
		t.Fatalf("got status %d, upstream called=%v", response.Code, called)
	}
}

func TestTokenizerPreflightUsesOnlyApprovedModelAndReturnsBoundedCount(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v1/chat/completions/input_tokens" ||
			r.Header.Get("Authorization") != "Bearer server-only-upstream-secret" {
			t.Errorf("unexpected tokenizer upstream request: path=%q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"response.input_tokens","input_tokens":321}`))
	}))
	defer upstream.Close()
	gateway := testGateway(upstream, map[string]map[string]bool{"owner-subject": {permissionChat: true}})
	body := `{"model":"model-installed-by-owner","messages":[{"role":"user","content":"hello"}],"stream":false}`
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions/input_tokens", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer valid-token")
	response := httptest.NewRecorder()
	gateway.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != `{"input_tokens":321}` || calls != 1 {
		t.Fatalf("tokenizer preflight failed: status=%d calls=%d body=%s", response.Code, calls, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions/input_tokens", strings.NewReader(
		`{"model":"unapproved-model","messages":[{"role":"user","content":"hello"}],"stream":false}`))
	request.Header.Set("Authorization", "Bearer valid-token")
	response = httptest.NewRecorder()
	gateway.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || calls != 1 {
		t.Fatalf("unapproved model reached tokenizer: status=%d calls=%d", response.Code, calls)
	}
}

func TestAuthorizationAuditLogsDecisionWithoutCredentialsOrRawSubject(t *testing.T) {
	if got := routeName("/v1/conversations/4e5f8a57-927f-4202-91f3-03f7de31afd3"); got != "/v1/conversations/{id}" {
		t.Fatalf("conversation identifier leaked into route label: %q", got)
	}
	var output bytes.Buffer
	previousWriter := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(previousWriter) })

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"data":[]}`)
	}))
	defer upstream.Close()
	gateway := testGateway(upstream, map[string]map[string]bool{"owner-subject": {permissionModels: true}})
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("Authorization", "Bearer valid-token")
	response := httptest.NewRecorder()
	gateway.Handler().ServeHTTP(response, request)

	logOutput := output.String()
	if response.Code != http.StatusOK || !strings.Contains(logOutput, "auth_event") ||
		!strings.Contains(logOutput, "request_id="+response.Header().Get("X-Request-ID")) ||
		!strings.Contains(logOutput, "permission=ai.models outcome=allow") ||
		!strings.Contains(logOutput, "subject_hash="+auditSubjectID("owner-subject")) ||
		strings.Contains(logOutput, "owner-subject") || strings.Contains(logOutput, "valid-token") {
		t.Fatalf("authorization audit event missing or sensitive data leaked: status=%d logs=%q", response.Code, logOutput)
	}

	output.Reset()
	deniedRequest := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	deniedRequest.Header.Set("Authorization", "Bearer valid-token")
	deniedResponse := httptest.NewRecorder()
	gateway.Handler().ServeHTTP(deniedResponse, deniedRequest)
	logOutput = output.String()
	if deniedResponse.Code != http.StatusForbidden ||
		!strings.Contains(logOutput, "request_id="+deniedResponse.Header().Get("X-Request-ID")) ||
		!strings.Contains(logOutput, "permission=ai.chat outcome=deny reason=permission_denied") ||
		!strings.Contains(logOutput, "subject_hash="+auditSubjectID("owner-subject")) ||
		strings.Contains(logOutput, "owner-subject") || strings.Contains(logOutput, "valid-token") {
		t.Fatalf("permission-denial audit event missing or sensitive data leaked: status=%d logs=%q", deniedResponse.Code, logOutput)
	}
}

func TestSubjectPermissionsAreCheckedServerSide(t *testing.T) {
	called := false
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer upstream.Close()
	gateway := testGateway(upstream, map[string]map[string]bool{"owner-subject": {permissionModels: true}})
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"model-installed-by-owner","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	request.Header.Set("Authorization", "Bearer valid-token")
	response := httptest.NewRecorder()
	gateway.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || called {
		t.Fatalf("got status %d, upstream called=%v", response.Code, called)
	}
}

func TestModelsAreFilteredAndClientTokenIsNotForwardedUpstream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer server-only-upstream-secret" {
			t.Errorf("unexpected upstream authorization header")
		}
		if strings.Contains(r.Header.Get("Authorization"), "valid-token") {
			t.Errorf("client token was forwarded upstream")
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"model-installed-by-owner"},{"id":"unapproved-model"}]}`)
	}))
	defer upstream.Close()
	gateway := testGateway(upstream, map[string]map[string]bool{"owner-subject": {permissionModels: true}})
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("Authorization", "Bearer valid-token")
	response := httptest.NewRecorder()
	gateway.Handler().ServeHTTP(response, request)
	var result modelEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || len(result.Data) != 1 || result.Data[0].ID != "model-installed-by-owner" {
		t.Fatalf("unexpected filtered model result: status=%d result=%+v", response.Code, result)
	}
}

func TestModelListRequestsSharePerSubjectRateLimit(t *testing.T) {
	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"id":"model-installed-by-owner"}]}`)
	}))
	defer upstream.Close()
	gateway := testGateway(upstream, map[string]map[string]bool{"owner-subject": {permissionModels: true}})
	gateway.cfg.RequestsPerMinute = 1
	for i, expected := range []int{http.StatusOK, http.StatusTooManyRequests} {
		request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		request.Header.Set("Authorization", "Bearer valid-token")
		response := httptest.NewRecorder()
		gateway.Handler().ServeHTTP(response, request)
		if response.Code != expected {
			t.Fatalf("request %d returned %d, want %d", i+1, response.Code, expected)
		}
	}
	if upstreamCalls != 1 {
		t.Fatalf("model upstream received %d calls after rate limit, want 1", upstreamCalls)
	}
}

func TestModelsRejectsOversizedUpstreamJSON(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"data":[{"id":"model-installed-by-owner"}]}`+strings.Repeat(" ", 1<<20))
	}))
	defer upstream.Close()
	gateway := testGateway(upstream, map[string]map[string]bool{"owner-subject": {permissionModels: true}})
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("Authorization", "Bearer valid-token")
	response := httptest.NewRecorder()
	gateway.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadGateway || strings.Contains(response.Body.String(), "model-installed-by-owner") {
		t.Fatalf("oversized upstream response was accepted or leaked: status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestUnapprovedModelAndToolMessagesAreRejectedBeforeUpstream(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	defer upstream.Close()
	gateway := testGateway(upstream, map[string]map[string]bool{"owner-subject": {permissionChat: true}})
	for _, body := range []string{
		`{"model":"unapproved-model","messages":[{"role":"user","content":"hi"}],"stream":true}`,
		`{"model":"model-installed-by-owner","messages":[{"role":"tool","content":"run command"}],"stream":true}`,
		`{"model":"model-installed-by-owner","messages":[{"role":"user","content":"hi"}],"stream":true,"tools":[{"type":"function"}]}`,
		`{"model":"model-installed-by-owner","messages":[{"role":"user","content":"hi"}],"stream":true} {"extra":true}`,
	} {
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer valid-token")
		response := httptest.NewRecorder()
		gateway.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("request returned status %d; wanted 400", response.Code)
		}
	}
	if calls != 0 {
		t.Fatalf("upstream was called %d times for rejected requests", calls)
	}
}

func TestValidateCompletionAllowsOnlyPairedSavedSnapshotToolExchange(t *testing.T) {
	allowed := map[string]bool{"model-installed-by-owner": true}
	definition := apiToolDefinition{Type: "function"}
	definition.Function.Name = "nexus_get_saved_host_snapshot"
	definition.Function.Description = "Read only an already-saved snapshot."
	definition.Function.Parameters = json.RawMessage(`{"type":"object","properties":{"device_id":{"type":"string","enum":["device-uuid"]}},"required":["device_id"],"additionalProperties":false}`)
	valid := completionRequest{Model: "model-installed-by-owner", Stream: true, Tools: []apiToolDefinition{definition}, ToolChoice: "auto", Messages: []apiMessage{
		{Role: "user", Content: json.RawMessage(`"Summarize current host status"`)},
		{Role: "assistant", Content: json.RawMessage(`"I can check the saved snapshot."`), ToolCalls: []apiToolCall{{ID: "call-1", Type: "function", Function: apiToolFunction{Name: "nexus_get_saved_host_snapshot", Arguments: json.RawMessage(`{"device_id":"device-uuid"}`)}}}},
		{Role: "tool", Content: json.RawMessage(`"CPU: 14%, memory: 42%"`), ToolCallID: "call-1"},
	}}
	if err := validateCompletion(valid, allowed); err != nil {
		t.Fatalf("valid paired snapshot tool exchange was rejected: %v", err)
	}
	parallel := false
	valid.ParallelToolCalls = &parallel
	if err := validateCompletion(valid, allowed); err != nil {
		t.Fatalf("explicitly disabled parallel tool calls were rejected: %v", err)
	}
	parallel = true
	if err := validateCompletion(valid, allowed); err == nil {
		t.Fatal("parallel tool calls must fail closed")
	}
	valid.ParallelToolCalls = nil

	encoded, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	invalid := make([]completionRequest, 4)
	for index := range invalid {
		if err := json.Unmarshal(encoded, &invalid[index]); err != nil {
			t.Fatal(err)
		}
	}
	invalid[0].Messages[2].ToolCallID = "wrong-call"
	invalid[1].Messages = invalid[1].Messages[:2]
	invalid[2].Messages[1].ToolCalls[0].Function.Arguments = json.RawMessage(`{"device_id":"not-enumerated"}`)
	invalid[3].Tools[0].Function.Name = "run_shell_command"
	for index, candidate := range invalid {
		if err := validateCompletion(candidate, allowed); err == nil {
			t.Errorf("invalid tool exchange %d was accepted", index)
		}
	}
}

func TestValidateCompletionAllowsOnlyPairedSavedServiceInventoryToolExchange(t *testing.T) {
	allowed := map[string]bool{"model-installed-by-owner": true}
	serviceTool := apiToolDefinition{Type: "function"}
	serviceTool.Function.Name = "nexus_get_saved_service_inventory"
	serviceTool.Function.Description = "Read only a previously saved service list."
	serviceTool.Function.Parameters = json.RawMessage(`{"type":"object","properties":{"device_id":{"type":"string","enum":["device-uuid"]}},"required":["device_id"],"additionalProperties":false}`)
	snapshotTool := serviceTool
	snapshotTool.Function.Name = "nexus_get_saved_host_snapshot"
	snapshotTool.Function.Parameters = json.RawMessage(`{"type":"object","properties":{"device_id":{"type":"string","enum":["other-device"]}},"required":["device_id"],"additionalProperties":false}`)
	valid := completionRequest{Model: "model-installed-by-owner", Stream: true,
		Tools: []apiToolDefinition{serviceTool, snapshotTool}, ToolChoice: "auto", Messages: []apiMessage{
			{Role: "assistant", Content: json.RawMessage(`"I can share the saved service list if you approve."`), ToolCalls: []apiToolCall{{ID: "call-services", Type: "function", Function: apiToolFunction{Name: serviceTool.Function.Name, Arguments: json.RawMessage(`{"device_id":"device-uuid"}`)}}}},
			{Role: "tool", Content: json.RawMessage(`"[{\"name\":\"ssh.service\",\"active\":\"active\"}]"`), ToolCallID: "call-services"},
		}}
	if err := validateCompletion(valid, allowed); err != nil {
		t.Fatalf("valid paired service inventory tool exchange was rejected: %v", err)
	}
	valid.Tools[0].Function.Name = "run_shell_command"
	if err := validateCompletion(valid, allowed); err == nil {
		t.Fatal("unsupported shell tool was accepted")
	}
}

func TestValidateCompletionAllowsOnlyBoundedUserJPEGAttachments(t *testing.T) {
	allowed := map[string]bool{"vision-model": true}
	jpeg := base64.StdEncoding.EncodeToString([]byte{0xFF, 0xD8, 0xFF, 0x01, 0xFF, 0xD9})
	valid := completionRequest{Model: "vision-model", Messages: []apiMessage{{Role: "user", Content: json.RawMessage(
		`[{"type":"text","text":"describe it"},{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,` + jpeg + `","detail":"auto"}}]`)}}, Stream: true}
	if err := validateCompletion(valid, allowed); err != nil {
		t.Fatalf("valid bounded user image was rejected: %v", err)
	}
	validMulti := completionRequest{Model: "vision-model", Messages: []apiMessage{{Role: "user", Content: json.RawMessage(
		`[{"type":"text","text":"compare"},{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,` + jpeg + `"}},{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,` + jpeg + `"}}]`)}}, Stream: true}
	if err := validateCompletion(validMulti, allowed); err != nil {
		t.Fatalf("valid multi-image user message was rejected: %v", err)
	}

	invalid := []completionRequest{
		{Model: "vision-model", Messages: []apiMessage{{Role: "assistant", Content: valid.Messages[0].Content}}},
		{Model: "vision-model", Messages: []apiMessage{{Role: "user", Content: json.RawMessage(
			`[{"type":"text","text":"x"},{"type":"image_url","image_url":{"url":"data:image/png;base64,` + jpeg + `"}}]`)}}},
		{Model: "vision-model", Messages: []apiMessage{{Role: "user", Content: json.RawMessage(
			`[{"type":"text","text":"x"},{"type":"image_url","image_url":{"url":"https://example.test/image.jpg"}}]`)}}},
		{Model: "vision-model", Messages: []apiMessage{{Role: "user", Content: json.RawMessage(`null`)}}},
	}
	for index, request := range invalid {
		if err := validateCompletion(request, allowed); err == nil {
			t.Errorf("invalid image request %d was accepted", index)
		}
	}

	oversized := make([]byte, maxImageBytes+1)
	oversized[0], oversized[1], oversized[2] = 0xFF, 0xD8, 0xFF
	oversized[len(oversized)-2], oversized[len(oversized)-1] = 0xFF, 0xD9
	dataURL := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(oversized)
	tooLarge := completionRequest{Model: "vision-model", Messages: []apiMessage{{Role: "user", Content: json.RawMessage(
		`[{"type":"text","text":"x"},{"type":"image_url","image_url":{"url":"` + dataURL + `"}}]`)}}}
	if err := validateCompletion(tooLarge, allowed); err == nil {
		t.Fatal("oversized image was accepted")
	}

	boundedImage := make([]byte, maxImageBytes)
	boundedImage[0], boundedImage[1], boundedImage[2] = 0xFF, 0xD8, 0xFF
	boundedImage[len(boundedImage)-2], boundedImage[len(boundedImage)-1] = 0xFF, 0xD9
	boundedURL := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(boundedImage)
	tooManyParts := `[{"type":"text","text":"x"}` +
		`,{"type":"image_url","image_url":{"url":"` + boundedURL + `"}}` +
		`,{"type":"image_url","image_url":{"url":"` + boundedURL + `"}}` +
		`,{"type":"image_url","image_url":{"url":"` + boundedURL + `"}}` +
		`,{"type":"image_url","image_url":{"url":"` + boundedURL + `"}}]`
	tooMany := completionRequest{Model: "vision-model", Messages: []apiMessage{{Role: "user", Content: json.RawMessage(tooManyParts)}}}
	if err := validateCompletion(tooMany, allowed); err == nil {
		t.Fatal("more than three image attachments were accepted")
	}
	combinedTooLarge := completionRequest{Model: "vision-model", Messages: []apiMessage{{Role: "user", Content: json.RawMessage(
		`[{"type":"text","text":"x"},{"type":"image_url","image_url":{"url":"` + boundedURL + `"}},{"type":"image_url","image_url":{"url":"` + boundedURL + `"}},{"type":"image_url","image_url":{"url":"` + boundedURL + `"}}]`)}}}
	if err := validateCompletion(combinedTooLarge, allowed); err == nil {
		t.Fatal("combined image payload over one MiB was accepted")
	}
}

func TestStreamingResponseIsForwardedWithoutBufferingOrUpstreamDetails(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected upstream path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\n")
		w.(http.Flusher).Flush()
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()
	gateway := testGateway(upstream, map[string]map[string]bool{"owner-subject": {permissionChat: true}})
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"model-installed-by-owner","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	request.Header.Set("Authorization", "Bearer valid-token")
	response := httptest.NewRecorder()
	gateway.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "text/event-stream" ||
		response.Header().Get("X-Accel-Buffering") != "no" ||
		!strings.Contains(response.Body.String(), `"content":"first"`) || !strings.Contains(response.Body.String(), "[DONE]") {
		t.Fatalf("unexpected streaming response: status=%d headers=%v body=%q", response.Code, response.Header(), response.Body.String())
	}
}

func TestStreamingResponseHasTotalByteLimit(t *testing.T) {
	upstreamBody := bytes.Repeat([]byte("x"), maxStreamResponseBytes+1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(upstreamBody)
	}))
	defer upstream.Close()
	gateway := testGateway(upstream, map[string]map[string]bool{"owner-subject": {permissionChat: true}})
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"model-installed-by-owner","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	request.Header.Set("Authorization", "Bearer valid-token")
	response := httptest.NewRecorder()
	gateway.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.Len() != maxStreamResponseBytes {
		t.Fatalf("stream response was not bounded: status=%d bytes=%d", response.Code, response.Body.Len())
	}
}

func TestUpstreamErrorBodyIsNotReturnedToClient(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "provider token=private-detail", http.StatusUnauthorized)
	}))
	defer upstream.Close()
	gateway := testGateway(upstream, map[string]map[string]bool{"owner-subject": {permissionModels: true}})
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("Authorization", "Bearer valid-token")
	response := httptest.NewRecorder()
	gateway.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadGateway || strings.Contains(response.Body.String(), "private-detail") {
		t.Fatalf("upstream error leaked or was misclassified: %d %q", response.Code, response.Body.String())
	}
}

func TestPrivateUpstreamAndLoopbackListenerPolicy(t *testing.T) {
	for _, value := range []string{
		"http://192.168.1.50:8081/v1",
		"http://100.64.20.1:8081/v1",
		"https://ai.example.com/v1",
	} {
		parsed, _ := url.Parse(value)
		if !validUpstreamURL(parsed) {
			t.Errorf("expected upstream URL to be accepted: %s", value)
		}
	}
	for _, value := range []string{
		"http://8.8.8.8/v1",
		"http://ai.example.com/v1",
		"http://user:secret@192.168.1.2/v1",
		"http://192.168.1.2/v1?token=x",
	} {
		parsed, _ := url.Parse(value)
		if validUpstreamURL(parsed) {
			t.Errorf("expected upstream URL to be rejected: %s", value)
		}
	}
	if !loopbackListenAddress("127.0.0.1:8787") || !loopbackListenAddress("[::1]:8787") || loopbackListenAddress("0.0.0.0:8787") {
		t.Fatal("listener loopback policy is incorrect")
	}
}

func TestGatewayAudienceRequiresCleanHTTPSResourceURI(t *testing.T) {
	for _, value := range []string{"https://api.example.test", "https://api.example.test/v1/resource"} {
		if !validOIDCAudience(value) {
			t.Errorf("expected audience to be accepted: %s", value)
		}
	}
	for _, value := range []string{"http://api.example.test", "https://user@api.example.test", "https://api.example.test?token=x", "https://api.example.test/#fragment", ""} {
		if validOIDCAudience(value) {
			t.Errorf("expected audience to be rejected: %s", value)
		}
	}
}

func TestParseSubjectPermissionsRejectsAmbiguousPolicy(t *testing.T) {
	for name, raw := range map[string]string{
		"duplicate subject key":           `{"owner":["ai.chat"],"owner":["ai.search"]}`,
		"whitespace-normalized collision": `{"owner":["ai.chat"]," owner ":["ai.search"]}`,
		"duplicate permission":            `{"owner":["ai.chat","ai.chat"]}`,
		"unsupported permission":          `{"owner":["admin"]}`,
		"null permission list":            `{"owner":null}`,
		"empty policy":                    `{}`,
		"non-object policy":               `[]`,
		"trailing value":                  `{"owner":["ai.chat"]} {}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseSubjectPermissions(raw); err == nil {
				t.Fatalf("ambiguous or invalid subject policy was accepted: %s", raw)
			}
		})
	}
}

func TestParseSubjectPermissionsBuildsExactPermissionSets(t *testing.T) {
	got, err := parseSubjectPermissions(`{"owner-subject":["ai.chat","ai.search"],"observer":["ai.models"]}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !got["owner-subject"][permissionChat] || !got["owner-subject"][permissionSearch] ||
		got["owner-subject"][permissionHistory] || !got["observer"][permissionModels] || got["observer"][permissionChat] {
		t.Fatalf("unexpected parsed permissions: %#v", got)
	}
}

func TestMetadataPollingCannotExhaustChatBudget(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	defer upstream.Close()
	g := testGateway(upstream, map[string]map[string]bool{"owner-subject": {permissionModels: true, permissionNexusModelRead: true, permissionChat: true}})
	g.cfg.RequestsPerMinute = 1
	g.cfg.MetadataRequestsPerMinute = 2
	call := func(permission, method string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/fixture", nil)
		r.Header.Set("Authorization", "Bearer valid-token")
		w := httptest.NewRecorder()
		g.authorize(permission, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })).ServeHTTP(w, r)
		return w
	}
	if call(permissionModels, "GET").Code != 200 || call(permissionNexusModelRead, "GET").Code != 200 {
		t.Fatal("metadata budget was not shared by model/status reads")
	}
	limited := call(permissionModels, "GET")
	if limited.Code != 429 || limited.Header().Get("Retry-After") != "60" {
		t.Fatal("metadata requests must remain bounded with retry guidance")
	}
	if call(permissionChat, "POST").Code != 200 {
		t.Fatal("read polling exhausted chat budget")
	}
	if call(permissionChat, "POST").Code != 429 {
		t.Fatal("chat limit was weakened")
	}
	if !g.allowRequestBudget("another-principal", true) || !g.allowRequestBudget("another-principal", false) {
		t.Fatal("principal budgets must remain isolated")
	}
}

// testPS5Gateway wires a gateway to the mock PS5: chat relays to inference, control and health go to mock.Control.
// The bearer is a credential file in a temporary CREDENTIALS_DIRECTORY, as systemd would provide it.
func testPS5Gateway(t *testing.T, mock *mockPS5, inference *httptest.Server) *Gateway {
	t.Helper()
	credentials := t.TempDir()
	if err := os.WriteFile(filepath.Join(credentials, "ps5-token"), []byte(mockPS5Token), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CREDENTIALS_DIRECTORY", credentials)
	parsed, _ := url.Parse(inference.URL)
	parsed.Path = "/v1"
	return NewGateway(Config{
		SubjectPermissions:        map[string]map[string]bool{"alice": {permissionChat: true, permissionModels: true}, "bob": {permissionChat: true}},
		Upstream:                  parsed,
		UpstreamBearerToken:       mockPS5Token,
		AllowedModels:             map[string]bool{"model-installed-by-owner": true},
		RequestTimeout:            5 * time.Second,
		RequestsPerMinute:         100,
		MetadataRequestsPerMinute: 100,
		MaximumConcurrentAI:       1,
		PS5ControlURL:             mock.Control.URL,
		PS5MediaURL:               mock.Media.URL,
		PS5TokenCredential:        "ps5-token",
		PS5PollInterval:           time.Second,
	}, fakeVerifier{subjects: map[string]string{"alice-token": "alice", "bob-token": "bob"}})
}

func ps5ChatRequest(token, prompt string) *http.Request {
	body := `{"model":"model-installed-by-owner","messages":[{"role":"user","content":"` + prompt + `"}],"stream":false}`
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	return request
}

func TestChatQueued(t *testing.T) {
	mock := newMockPS5(t)
	mock.Set(func(s *mockPS5Switches) { s.Slow = 200 * time.Millisecond })
	routes := mock.inferenceRoutes()
	var (
		mu            sync.Mutex
		running, peak int
	)
	// Count requests inside the PS5 stand-in: a second chat must wait, so the peak must stay at one.
	inference := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		running++
		peak = max(peak, running)
		mu.Unlock()
		defer func() {
			mu.Lock()
			running--
			mu.Unlock()
		}()
		routes.ServeHTTP(w, r)
	}))
	defer inference.Close()

	gateway := testPS5Gateway(t, mock, inference)
	if state := gateway.ps5.health.Poll(context.Background()); !state.Online || state.Engine != "llm" {
		t.Fatalf("PS5 not online for chat: %+v", state)
	}
	usageDir := t.TempDir()
	usage, err := newUsageLog(usageDir, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	gateway.usage = usage
	handler := gateway.Handler()

	const prompt = "private prompt marker"
	codes := make([]int, 2)
	var wg sync.WaitGroup
	for i, token := range []string{"alice-token", "bob-token"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, ps5ChatRequest(token, prompt))
			codes[i] = response.Code
		}()
	}
	wg.Wait()
	if codes[0] != http.StatusOK || codes[1] != http.StatusOK {
		t.Fatalf("chat statuses = %v, want both 200 (second chat waits in the queue)", codes)
	}
	mu.Lock()
	gotPeak := peak
	mu.Unlock()
	if gotPeak != 1 {
		t.Fatalf("peak concurrent PS5 chats = %d, want 1", gotPeak)
	}
	files, err := filepath.Glob(filepath.Join(usageDir, "usage-*.jsonl"))
	if err != nil || len(files) != 1 {
		t.Fatalf("usage files = %v, err = %v", files, err)
	}
	logged, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(string(logged), "\n"); lines != 2 {
		t.Fatalf("usage lines = %d, want 2", lines)
	}
	if strings.Contains(string(logged), prompt) {
		t.Fatal("usage log contains prompt text")
	}
}

func TestChatOffline(t *testing.T) {
	mock := newMockPS5(t)
	mock.Set(func(s *mockPS5Switches) { s.Offline = true })
	var reached atomic.Bool
	inference := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached.Store(true) }))
	defer inference.Close()

	gateway := testPS5Gateway(t, mock, inference)
	if state := gateway.ps5.health.Poll(context.Background()); state.Online {
		t.Fatalf("PS5 reported online while its control API is down: %+v", state)
	}
	response := httptest.NewRecorder()
	gateway.Handler().ServeHTTP(response, ps5ChatRequest("alice-token", "hello"))
	if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") == "" ||
		strings.TrimSpace(response.Body.String()) != `{"error":{"message":"PS5 is offline"}}` {
		t.Fatalf("offline chat: status=%d retry-after=%q body=%q", response.Code, response.Header().Get("Retry-After"), response.Body.String())
	}
	if reached.Load() {
		t.Fatal("offline chat reached the PS5 inference server")
	}
	status := httptest.NewRecorder()
	statusRequest := httptest.NewRequest(http.MethodGet, "/v1/ai/status", nil)
	statusRequest.Header.Set("Authorization", "Bearer alice-token")
	gateway.Handler().ServeHTTP(status, statusRequest)
	if status.Code != http.StatusOK || !strings.Contains(status.Body.String(), `"online":false`) ||
		!strings.Contains(status.Body.String(), `"reason":"ps5_offline"`) {
		t.Fatalf("status while offline: %d %s", status.Code, status.Body.String())
	}
}

func TestChatQueueFullReturns429WithQueueMessage(t *testing.T) {
	mock := newMockPS5(t)
	inference := httptest.NewServer(mock.inferenceRoutes())
	defer inference.Close()
	gateway := testPS5Gateway(t, mock, inference)
	if state := gateway.ps5.health.Poll(context.Background()); !state.Online {
		t.Fatalf("PS5 not online: %+v", state)
	}
	// Fill alice's per-subject allowance with jobs that never start, so her next chat is refused.
	for range queueDefaultPerSubject {
		if _, err := gateway.queue.Enqueue(context.Background(), "alice", "llm", "model-installed-by-owner"); err != nil {
			t.Fatal(err)
		}
	}
	response := httptest.NewRecorder()
	gateway.Handler().ServeHTTP(response, ps5ChatRequest("alice-token", "hello"))
	if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") == "" ||
		!strings.Contains(response.Body.String(), "PS5 queue is full") {
		t.Fatalf("queue full: status=%d retry-after=%q body=%q", response.Code, response.Header().Get("Retry-After"), response.Body.String())
	}
}

func TestAIPermissionsAndQuotaDefaultDeny(t *testing.T) {
	permissions, err := parseSubjectPermissions(`{"owner":["ai.image","ai.video","ai.audio","ai.voice","ai.voice.clone"]}`)
	if err != nil || len(permissions["owner"]) != 5 {
		t.Fatalf("media permissions rejected: %v", err)
	}
	// A permission with no quota rule is refused, and an exhausted rule raises a quota error without an alert sink.
	gateway := &Gateway{quota: newQuotaMeter(quotaPolicy{quotaPermissionVideo: {Unit: quotaUnitRequests, Limit: 1}}, time.Now)}
	if _, err := gateway.consumeQuota("owner", quotaPermissionImage, 1); !errors.Is(err, errQuotaNotConfigured) {
		t.Fatalf("unconfigured permission: err = %v", err)
	}
	if _, err := gateway.consumeQuota("owner", quotaPermissionVideo, 1); err != nil {
		t.Fatalf("first video within quota: %v", err)
	}
	if _, err := gateway.consumeQuota("owner", quotaPermissionVideo, 1); !errors.Is(err, errQuotaExceeded) {
		t.Fatalf("second video: err = %v, want quota exceeded", err)
	}
}

func TestMediaRoutesRegistered(t *testing.T) {
	const catchAll = `{"error":{"message":"not found"}}`
	routes := []struct {
		method, path, permission, body string
	}{
		{http.MethodPost, "/v1/audio/transcriptions", quotaPermissionAudio, "{}"},
		{http.MethodPost, "/v1/audio/speech", quotaPermissionAudio, "{}"},
		{http.MethodPost, "/v1/voice/chat", quotaPermissionVoice, "{}"},
		{http.MethodPost, "/v1/images/generations", quotaPermissionImage, "{}"},
		{http.MethodPost, "/v1/videos/generations", quotaPermissionVideo, "{}"},
		{http.MethodGet, "/v1/ai/jobs/0123456789abcdef0123456789abcdef/result", permissionModels, ""},
	}
	send := func(handler http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}

	// PS5 configured: the media subject holds every media permission; bob holds chat only.
	mock := newMockPS5(t)
	inference := httptest.NewServer(mock.inferenceRoutes())
	t.Cleanup(inference.Close)
	gateway := testPS5Gateway(t, mock, inference)
	if state := gateway.ps5.health.Poll(context.Background()); !state.Online {
		t.Fatalf("PS5 not online: %+v", state)
	}
	gateway.cfg.SubjectPermissions["media"] = map[string]bool{permissionModels: true, quotaPermissionAudio: true,
		quotaPermissionVoice: true, quotaPermissionImage: true, quotaPermissionVideo: true}
	gateway.verifier = fakeVerifier{subjects: map[string]string{"media-token": "media", "bob-token": "bob"}}
	handler := gateway.Handler()
	for _, route := range routes {
		if response := send(handler, route.method, route.path, "bob-token", route.body); response.Code != http.StatusForbidden {
			t.Errorf("%s %s without %s: status = %d, want 403", route.method, route.path, route.permission, response.Code)
		}
		response := send(handler, route.method, route.path, "media-token", route.body)
		if response.Code == http.StatusForbidden || response.Code == http.StatusMethodNotAllowed ||
			strings.TrimSpace(response.Body.String()) == catchAll {
			t.Errorf("%s %s with %s is not routed: status = %d", route.method, route.path, route.permission, response.Code)
		}
	}

	// PS5 not configured: the same paths fall through to the generic 404, even for an authorized subject.
	upstream := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(upstream.Close)
	plain := testGateway(upstream, map[string]map[string]bool{"owner-subject": {permissionModels: true,
		quotaPermissionAudio: true, quotaPermissionVoice: true, quotaPermissionImage: true, quotaPermissionVideo: true}}).Handler()
	for _, route := range routes {
		response := send(plain, route.method, route.path, "valid-token", route.body)
		if response.Code != http.StatusNotFound || strings.TrimSpace(response.Body.String()) != catchAll {
			t.Errorf("%s %s without PS5: status = %d, want generic 404", route.method, route.path, response.Code)
		}
	}

	// Per-route request body caps (D-6 and plan U-51): each handler bounds its own body.
	caps := map[string][2]int{
		"transcription": {sttMaxBodyBytes, 16 << 20},
		"speech":        {ttsMaxBodyBytes, 4 << 20},
		"voice":         {mediaVoiceMaxBodyBytes, 16 << 20},
		"image":         {mediaImageMaxRequestBytes, 12 << 20},
		"video":         {mediaVideoMaxBodyBytes, 16 << 20},
	}
	for name, limit := range caps {
		if limit[0] != limit[1] {
			t.Errorf("%s request cap = %d, want %d", name, limit[0], limit[1])
		}
	}
}
