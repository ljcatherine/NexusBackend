package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func aiTestEnqueue(t *testing.T, q *queueManager, subject, engine, model string) *queueJob {
	t.Helper()
	job, err := q.Enqueue(context.Background(), subject, engine, model)
	if err != nil {
		t.Fatalf("enqueue %s: %v", subject, err)
	}
	return job
}

func aiTestSubject(name string) func(*http.Request) string {
	return func(*http.Request) string { return name }
}

func aiTestGet(h http.HandlerFunc, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func aiTestDecode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return m
}

func aiTestHealth(s ps5State) func() ps5State {
	return func() ps5State { return s }
}

func TestAIStatusOtherSubjectJobIsNotFound(t *testing.T) {
	q := newQueueManager(0, 0)
	aiTestEnqueue(t, q, "alice", "llm", "m")
	theirs := aiTestEnqueue(t, q, "bob", "llm", "m")
	h := aiJobHandler(q, aiTestSubject("alice"))

	other := aiTestGet(h, "/v1/ai/jobs/"+theirs.ID)
	unknown := aiTestGet(h, "/v1/ai/jobs/ffffffffffffffffffffffffffffffff")
	malformed := aiTestGet(h, "/v1/ai/jobs/NOT-A-JOB")
	for name, rec := range map[string]*httptest.ResponseRecorder{"other": other, "unknown": unknown, "malformed": malformed} {
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s: status %d, want 404", name, rec.Code)
		}
	}
	if other.Body.String() != unknown.Body.String() || malformed.Body.String() != unknown.Body.String() {
		t.Fatalf("404 bodies differ: other=%q unknown=%q", other.Body.String(), unknown.Body.String())
	}
	if strings.Contains(other.Body.String(), "bob") || strings.Contains(other.Body.String(), theirs.ID) {
		t.Fatalf("404 leaks another subject's job: %q", other.Body.String())
	}
}

func TestAIStatusOwnJobSnapshot(t *testing.T) {
	q := newQueueManager(0, 0)
	mine := aiTestEnqueue(t, q, "alice", "llm", "m")
	rec := aiTestGet(aiJobHandler(q, aiTestSubject("alice")), "/v1/ai/jobs/"+mine.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	m := aiTestDecode(t, rec)
	if m["id"] != mine.ID || m["state"] != "queued" || m["position"] != float64(1) || m["engine"] != "llm" {
		t.Fatalf("unexpected snapshot: %v", m)
	}
	if _, ok := m["subject"]; ok {
		t.Fatalf("snapshot must not include subject: %v", m)
	}
}

func TestAIStatusOfflineResponse(t *testing.T) {
	q := newQueueManager(0, 0)
	h := aiStatusHandler(aiStatusDeps{health: aiTestHealth(ps5State{Reason: "ps5_offline"}), queue: q}, aiTestSubject("alice"))
	m := aiTestDecode(t, aiTestGet(h, "/v1/ai/status"))
	if m["online"] != false || m["reason"] != "ps5_offline" {
		t.Fatalf("offline response: %v", m)
	}
	if _, ok := m["warning"]; ok {
		t.Fatalf("offline response must not carry a warning: %v", m)
	}
}

func TestAIStatusListsOwnJobsWithPositions(t *testing.T) {
	q := newQueueManager(0, 0)
	a1 := aiTestEnqueue(t, q, "alice", "llm", "m")
	aiTestEnqueue(t, q, "bob", "llm", "m")
	a2 := aiTestEnqueue(t, q, "alice", "llm", "m")
	h := aiStatusHandler(aiStatusDeps{health: aiTestHealth(ps5State{Online: true, Engine: "llm", Model: "m", Ready: true}), queue: q},
		aiTestSubject("alice"))
	rec := aiTestGet(h, "/v1/ai/status")
	m := aiTestDecode(t, rec)
	if m["queue_length"] != float64(3) {
		t.Fatalf("queue_length = %v, want 3", m["queue_length"])
	}
	jobs, _ := m["jobs"].([]any)
	if len(jobs) != 2 {
		t.Fatalf("jobs = %v, want the caller's 2 jobs", m["jobs"])
	}
	first, second := jobs[0].(map[string]any), jobs[1].(map[string]any)
	if first["id"] != a1.ID || first["position"] != float64(1) || second["id"] != a2.ID || second["position"] != float64(3) {
		t.Fatalf("unexpected jobs: %v", jobs)
	}
	if strings.Contains(rec.Body.String(), "bob") {
		t.Fatalf("status leaks another subject: %s", rec.Body.String())
	}
}

func TestAIStatusWarningOnEngineSwitch(t *testing.T) {
	online := ps5State{Online: true, Engine: "llm", Model: "m", Ready: true}

	q := newQueueManager(0, 0)
	h := aiStatusHandler(aiStatusDeps{health: aiTestHealth(online), queue: q}, aiTestSubject("alice"))
	aiTestEnqueue(t, q, "alice", "media", "wan")
	if w, _ := aiTestDecode(t, aiTestGet(h, "/v1/ai/status"))["warning"].(string); !strings.Contains(w, "restarts the PS5") {
		t.Fatalf("expected engine switch warning, got %q", w)
	}

	q2 := newQueueManager(0, 0)
	h2 := aiStatusHandler(aiStatusDeps{health: aiTestHealth(online), queue: q2}, aiTestSubject("alice"))
	aiTestEnqueue(t, q2, "alice", "llm", "m")
	if _, ok := aiTestDecode(t, aiTestGet(h2, "/v1/ai/status"))["warning"]; ok {
		t.Fatalf("no switch needed, warning must be omitted")
	}
}

func TestAIStatusPickerPrefersMatchingEngine(t *testing.T) {
	now := func() time.Time { return time.Unix(1000, 0) }
	media := &queueJob{ID: "a", Subject: "x", Engine: "media", Model: "wan", Enqueued: now()}
	llm := &queueJob{ID: "b", Subject: "y", Engine: "llm", Model: "m", Enqueued: now()}

	pick := aiSchedulerPicker(aiTestHealth(ps5State{Online: true, Engine: "llm", Model: "m"}), now, 0)
	if got := pick([]*queueJob{media, llm}); got != 1 {
		t.Fatalf("picker = %d, want 1 (the llm job matching the running engine)", got)
	}
	pick = aiSchedulerPicker(aiTestHealth(ps5State{Online: true, Engine: "media", Model: "wan"}), now, 0)
	if got := pick([]*queueJob{llm, media}); got != 1 {
		t.Fatalf("picker = %d, want 1 (the media job matching the running engine)", got)
	}
}

func TestAIStatusPickerFairWithinEngine(t *testing.T) {
	base := time.Unix(1000, 0)
	q := newQueueManager(0, 0)
	q.now = func() time.Time { return base }
	q.picker = aiSchedulerPicker(aiTestHealth(ps5State{Online: true, Engine: "llm", Model: "m"}), func() time.Time { return base }, 0)
	ctx := context.Background()
	for _, s := range []string{"A", "A", "A", "B"} {
		if _, err := q.Enqueue(ctx, s, "llm", "m"); err != nil {
			t.Fatalf("enqueue %s: %v", s, err)
		}
	}
	var order []string
	for range 4 {
		job, err := q.Next(ctx)
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		order = append(order, job.Subject)
		q.Finish(job, queueStateDone)
	}
	if got := strings.Join(order, ","); got != "A,B,A,A" {
		t.Fatalf("served order = %s, want A,B,A,A", got)
	}
}

func TestAIStatusPickerStarvedJobWinsOutright(t *testing.T) {
	base := time.Unix(1000, 0)
	at := base
	q := newQueueManager(0, 0)
	q.now = func() time.Time { return at }
	q.picker = aiSchedulerPicker(aiTestHealth(ps5State{Online: true, Engine: "llm", Model: "m"}), func() time.Time { return at }, 10*time.Second)
	ctx := context.Background()
	starved, err := q.Enqueue(ctx, "x", "media", "wan")
	if err != nil {
		t.Fatalf("enqueue starved: %v", err)
	}
	at = base.Add(time.Minute)
	for _, s := range []string{"A", "A", "B"} {
		if _, err := q.Enqueue(ctx, s, "llm", "m"); err != nil {
			t.Fatalf("enqueue %s: %v", s, err)
		}
	}
	job, err := q.Next(ctx)
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if job != starved {
		t.Fatalf("first job = %s (%s), want the starved media job %s", job.Subject, job.Engine, starved.ID)
	}
}
