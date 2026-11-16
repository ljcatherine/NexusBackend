package main

import (
	"context"
	"errors"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

func queueTestEnqueue(t *testing.T, q *queueManager, ctx context.Context, subject string) *queueJob {
	t.Helper()
	job, err := q.Enqueue(ctx, subject, "llm", "test-model")
	if err != nil {
		t.Fatalf("enqueue %s: %v", subject, err)
	}
	return job
}

// queueTestWaitState polls until a job reaches want. Cancellation is applied from a context callback, so it is async.
func queueTestWaitState(t *testing.T, q *queueManager, id string, want queueState) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		s, ok := q.Status(id)
		if ok && s.State == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s state = %v (found=%v), want %s", id, s.State, ok, want)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestPS5QueueFairnessRunOrder(t *testing.T) {
	q := newQueueManager(0, 0)
	ctx := context.Background()
	for _, s := range []string{"A", "A", "A", "B"} {
		queueTestEnqueue(t, q, ctx, s)
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
		t.Fatalf("run order = %s, want A,B,A,A", got)
	}
}

func TestPS5QueueFairnessRoundRobinAcrossThreeSubjects(t *testing.T) {
	q := newQueueManager(5, 20)
	ctx := context.Background()
	for _, s := range []string{"A", "A", "B", "B", "C"} {
		queueTestEnqueue(t, q, ctx, s)
	}
	var order []string
	for range 5 {
		job, _ := q.Next(ctx)
		order = append(order, job.Subject)
		q.Finish(job, queueStateDone)
	}
	if got := strings.Join(order, ","); got != "A,B,C,A,B" {
		t.Fatalf("run order = %s, want A,B,C,A,B", got)
	}
}

func TestPS5QueueLimitsReturnErrQueueFull(t *testing.T) {
	ctx := context.Background()

	q := newQueueManager(0, 0)
	for range 3 {
		queueTestEnqueue(t, q, ctx, "A")
	}
	if _, err := q.Enqueue(ctx, "A", "llm", "m"); !errors.Is(err, errQueueFull) {
		t.Fatalf("4th job for subject A: err = %v, want errQueueFull", err)
	}
	queueTestEnqueue(t, q, ctx, "B") // another subject is still admitted

	g := newQueueManager(100, 0)
	for i := range 20 {
		queueTestEnqueue(t, g, ctx, string(rune('a'+i)))
	}
	if _, err := g.Enqueue(ctx, "z", "llm", "m"); !errors.Is(err, errQueueFull) {
		t.Fatalf("21st job overall: err = %v, want errQueueFull", err)
	}
}

func TestPS5QueueLimitCountsOnlyQueuedJobs(t *testing.T) {
	q := newQueueManager(1, 20)
	ctx := context.Background()
	first := queueTestEnqueue(t, q, ctx, "A")
	if _, err := q.Next(ctx); err != nil {
		t.Fatal(err)
	}
	// The running job does not occupy a queue slot, so one more queued job for A is accepted.
	queueTestEnqueue(t, q, ctx, "A")
	if _, err := q.Enqueue(ctx, "A", "llm", "m"); !errors.Is(err, errQueueFull) {
		t.Fatalf("second queued job for A: err = %v, want errQueueFull", err)
	}
	q.Finish(first, queueStateDone)
}

func TestPS5QueueCancelRemovesQueuedJob(t *testing.T) {
	q := newQueueManager(0, 0)
	bg := context.Background()
	cctx, cancel := context.WithCancel(bg)
	a := queueTestEnqueue(t, q, cctx, "A")
	b := queueTestEnqueue(t, q, bg, "B")

	cancel()
	queueTestWaitState(t, q, a.ID, queueStateCancelled)

	if s, _ := q.Status(a.ID); s.Position != 0 {
		t.Fatalf("cancelled job position = %d, want 0", s.Position)
	}
	if s, _ := q.Status(b.ID); s.Position != 1 {
		t.Fatalf("remaining job position = %d, want 1", s.Position)
	}
	job, err := q.Next(bg)
	if err != nil {
		t.Fatal(err)
	}
	if job.ID != b.ID {
		t.Fatalf("Next returned %s, want the surviving job %s", job.ID, b.ID)
	}
}

func TestPS5QueueCancelCancelsRunningContext(t *testing.T) {
	q := newQueueManager(0, 0)
	cctx, cancel := context.WithCancel(context.Background())
	job := queueTestEnqueue(t, q, cctx, "A")
	running, err := q.Next(context.Background())
	if err != nil || running != job {
		t.Fatalf("Next = %v, %v", running, err)
	}

	cancel()
	if job.Context().Err() == nil {
		t.Fatal("running job context not cancelled")
	}
	queueTestWaitState(t, q, job.ID, queueStateCancelled)

	// The slot stays held until the runner reports back, so nothing else starts.
	next := queueTestEnqueue(t, q, context.Background(), "B")
	blocked, cancelNext := context.WithCancel(context.Background())
	cancelNext()
	if _, err := q.Next(blocked); !errors.Is(err, context.Canceled) {
		t.Fatalf("Next while slot held: err = %v, want context.Canceled", err)
	}

	q.Finish(job, queueStateFailed)
	s, _ := q.Status(job.ID)
	if s.State != queueStateCancelled {
		t.Fatalf("state after Finish = %s, want cancelled", s.State)
	}
	started, err := q.Next(context.Background())
	if err != nil || started.ID != next.ID {
		t.Fatalf("Next after Finish = %v, %v; want job %s", started, err, next.ID)
	}
}

func TestPS5QueuePositionAndStates(t *testing.T) {
	q := newQueueManager(0, 0)
	ctx := context.Background()
	a1 := queueTestEnqueue(t, q, ctx, "A")
	b1 := queueTestEnqueue(t, q, ctx, "B")
	a2 := queueTestEnqueue(t, q, ctx, "A")

	for i, j := range []*queueJob{a1, b1, a2} {
		if s, _ := q.Status(j.ID); s.Position != i+1 || s.State != queueStateQueued {
			t.Fatalf("job %d: position=%d state=%s, want %d queued", i, s.Position, s.State, i+1)
		}
	}

	job, _ := q.Next(ctx)
	if job != a1 {
		t.Fatalf("first job = %s, want a1", job.ID)
	}
	if s, _ := q.Status(a1.ID); s.State != queueStateSwitching || s.Position != 0 {
		t.Fatalf("running job state=%s position=%d, want switching 0", s.State, s.Position)
	}
	if s, _ := q.Status(b1.ID); s.Position != 1 {
		t.Fatalf("b1 position after start = %d, want 1", s.Position)
	}

	for _, st := range []queueState{queueStateLoading, queueStateGenerating} {
		if !q.Advance(job, st) {
			t.Fatalf("Advance(%s) refused", st)
		}
	}
	if q.Advance(job, queueStateDone) {
		t.Fatal("Advance accepted a terminal state")
	}
	q.Finish(job, queueStateDone)
	if s, _ := q.Status(a1.ID); s.State != queueStateDone {
		t.Fatalf("final state = %s, want done", s.State)
	}
	if q.Advance(job, queueStateGenerating) {
		t.Fatal("Advance accepted a finished job")
	}
	if _, ok := q.Status("0123456789abcdef0123456789abcdef"); ok {
		t.Fatal("unknown job ID reported as found")
	}
}

func TestPS5QueueFinishedJobsAreBounded(t *testing.T) {
	q := newQueueManager(0, 0)
	ctx := context.Background()
	first := queueTestEnqueue(t, q, ctx, "A")
	if started, _ := q.Next(ctx); started != first {
		t.Fatal("unexpected job order")
	}
	q.Finish(first, queueStateDone)
	for range queueRetainFinished + 1 {
		job := queueTestEnqueue(t, q, ctx, "A")
		started, _ := q.Next(ctx)
		if started != job {
			t.Fatal("unexpected job order")
		}
		q.Finish(job, queueStateDone)
	}
	if _, ok := q.Status(first.ID); ok {
		t.Fatal("oldest finished job was not evicted")
	}
}

func TestPS5QueueJobIDFormat(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{32}$`)
	q := newQueueManager(100, 100)
	seen := map[string]bool{}
	for range 50 {
		job := queueTestEnqueue(t, q, context.Background(), "A")
		if !re.MatchString(job.ID) {
			t.Fatalf("job ID %q is not 32 lowercase hex characters", job.ID)
		}
		if seen[job.ID] {
			t.Fatalf("duplicate job ID %s", job.ID)
		}
		seen[job.ID] = true
	}
}

func TestPS5QueueStoresNoPromptText(t *testing.T) {
	typ := reflect.TypeOf(queueJob{})
	for i := range typ.NumField() {
		name := strings.ToLower(typ.Field(i).Name)
		for _, banned := range []string{"prompt", "message", "content", "audio", "body", "text"} {
			if strings.Contains(name, banned) {
				t.Fatalf("queueJob field %s looks like it stores request content", typ.Field(i).Name)
			}
		}
	}
}

func TestPS5QueuePickerHook(t *testing.T) {
	q := newQueueManager(0, 0)
	ctx := context.Background()
	queueTestEnqueue(t, q, ctx, "A")
	last := queueTestEnqueue(t, q, ctx, "B")
	q.picker = func(queued []*queueJob) int { return len(queued) - 1 }
	job, _ := q.Next(ctx)
	if job != last {
		t.Fatalf("picker ignored: got %s, want %s", job.ID, last.ID)
	}
}
