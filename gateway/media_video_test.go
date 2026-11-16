package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// mediaVideoTestGateway wires a gateway to a mock PS5 with a video quota, and a context that ends with the test.
func mediaVideoTestGateway(t *testing.T) (*Gateway, context.Context) {
	t.Helper()
	mediaRunnerSetTimers(t, 2*time.Millisecond, mediaSwitchTimeout)
	mock := newMockPS5(t)
	inference := httptest.NewServer(mock.inferenceRoutes())
	t.Cleanup(inference.Close)
	g := mediaRunnerGateway(t, mock.Control.URL, mock.Media.URL, inference.URL, 10)
	g.quota = newQuotaMeter(quotaPolicy{quotaPermissionVideo: {Unit: quotaUnitRequests, Limit: 10}}, time.Now)
	if s := g.ps5.health.Poll(context.Background()); !s.Online {
		t.Fatalf("PS5 not online: %+v", s)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return g, ctx
}

func mediaVideoRequest(ctx context.Context, method, target, body, subject string) *http.Request {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	return r.WithContext(context.WithValue(ctx, principalKey{}, subject))
}

// mediaVideoStart posts one generation request and returns the job ID from the 202 response.
func mediaVideoStart(t *testing.T, g *Gateway, ctx context.Context, body, subject string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	g.handleVideoGenerate(ctx).ServeHTTP(rec, mediaVideoRequest(ctx, http.MethodPost, "/v1/videos/generations", body, subject))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("generate: status=%d body=%q", rec.Code, rec.Body.String())
	}
	var accepted struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &accepted); err != nil || accepted.ID == "" {
		t.Fatalf("accepted body: %v %q", err, rec.Body.String())
	}
	return accepted.ID
}

// mediaVideoWaitDone polls GET /v1/ai/jobs/{id} as subject until the job is done.
func mediaVideoWaitDone(t *testing.T, g *Gateway, ctx context.Context, id, subject string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		rec := httptest.NewRecorder()
		aiJobHandler(g.queue, principalFromRequest)(rec, mediaVideoRequest(ctx, http.MethodGet, "/v1/ai/jobs/"+id, "", subject))
		var st struct {
			State string `json:"state"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &st) != nil {
			t.Fatalf("job status: %d %q", rec.Code, rec.Body.String())
		}
		switch st.State {
		case "done":
			return
		case "failed", "cancelled":
			t.Fatalf("job ended as %s", st.State)
		}
		if time.Now().After(deadline) {
			t.Fatalf("job still %s after the deadline", st.State)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestVideoGenerateRejectsFramesBeforeQueueing(t *testing.T) {
	g, ctx := mediaVideoTestGateway(t)
	for _, frames := range []string{"0", "2", "6", "37"} {
		body := `{"prompt":"a cat","width":128,"height":128,"frames":` + frames + `}`
		rec := httptest.NewRecorder()
		g.handleVideoGenerate(ctx).ServeHTTP(rec, mediaVideoRequest(ctx, http.MethodPost, "/v1/videos/generations", body, "alice"))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("frames=%s: status=%d body=%q", frames, rec.Code, rec.Body.String())
		}
	}
	g.queue.mu.Lock()
	queued, running := len(g.queue.queued), g.queue.running
	g.queue.mu.Unlock()
	if queued != 0 || running != nil {
		t.Fatalf("rejected requests reached the queue: queued=%d running=%v", queued, running != nil)
	}
	if _, err := g.quota.Consume("alice", quotaPermissionVideo, 10); err != nil {
		t.Fatalf("rejected requests consumed video quota: %v", err)
	}
}

func TestVideoGenerateQueuesAndReturnsResultOnce(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not on PATH")
	}
	g, ctx := mediaVideoTestGateway(t)
	body := `{"prompt":"a cat walks","width":128,"height":128,"frames":5,"format":"mp4"}`
	id := mediaVideoStart(t, g, ctx, body, "alice")
	mediaVideoWaitDone(t, g, ctx, id, "alice")

	get := func(subject string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		g.handleVideoResult().ServeHTTP(rec, mediaVideoRequest(ctx, http.MethodGet, "/v1/ai/jobs/"+id+"/result", "", subject))
		return rec
	}

	if rec := get("bob"); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), aiJobNotFound) {
		t.Fatalf("other subject: status=%d body=%q", rec.Code, rec.Body.String())
	}

	rec := get("alice")
	if rec.Code != http.StatusOK {
		t.Fatalf("owner result: status=%d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "video/mp4" {
		t.Fatalf("content type = %q, want video/mp4", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); cd != "inline" {
		t.Fatalf("content disposition = %q, want inline", cd)
	}
	if b := rec.Body.Bytes(); len(b) < 12 || string(b[4:8]) != "ftyp" {
		t.Fatalf("result is not an MP4 (%d bytes)", len(b))
	}

	if again := get("alice"); again.Code != http.StatusNotFound || !strings.Contains(again.Body.String(), aiJobNotFound) {
		t.Fatalf("second read: status=%d body=%q", again.Code, again.Body.String())
	}
}

func TestVideoResultRejectsMalformedID(t *testing.T) {
	g, ctx := mediaVideoTestGateway(t)
	for _, target := range []string{"/v1/ai/jobs/NOTHEX/result", "/v1/ai/jobs/" + strings.Repeat("a", 31) + "/result", "/v1/ai/jobs/" + strings.Repeat("a", 32)} {
		rec := httptest.NewRecorder()
		g.handleVideoResult().ServeHTTP(rec, mediaVideoRequest(ctx, http.MethodGet, target, "", "alice"))
		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), aiJobNotFound) {
			t.Fatalf("%s: status=%d body=%q", target, rec.Code, rec.Body.String())
		}
	}
}
