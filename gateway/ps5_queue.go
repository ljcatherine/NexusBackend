package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"slices"
	"sync"
	"time"
)

// errQueueFull is returned by Enqueue when a per-subject or global limit is reached. The HTTP layer maps it to 429.
var errQueueFull = errors.New("ps5 queue full")

type queueState string

const (
	queueStateQueued     queueState = "queued"
	queueStateSwitching  queueState = "switching"
	queueStateLoading    queueState = "loading"
	queueStateGenerating queueState = "generating"
	queueStateDone       queueState = "done"
	queueStateFailed     queueState = "failed"
	queueStateCancelled  queueState = "cancelled"
)

const (
	queueDefaultPerSubject = 3
	queueDefaultGlobal     = 20
	// queueRetainFinished bounds how many finished jobs stay reachable by ID.
	queueRetainFinished = 256
)

// queueJob is one PS5 request. It holds no prompt, messages or audio; the caller keeps those.
type queueJob struct {
	ID       string
	Subject  string
	Engine   string
	Model    string
	Enqueued time.Time // set by Enqueue; the scheduler uses it to cap waits

	ctx    context.Context
	cancel context.CancelFunc
	stop   func() bool
	state  queueState // guarded by queueManager.mu
	// servedSeq is the subject's last-served sequence, copied in by startNextLocked just before the picker runs.
	servedSeq uint64
}

// Context is cancelled when the caller's context ends or the job is finished.
func (j *queueJob) Context() context.Context { return j.ctx }

// queueSnapshot is a copy of a job's public state, safe to use after the lock is released.
type queueSnapshot struct {
	ID       string
	Subject  string
	Engine   string
	Model    string
	State    queueState
	Position int // 1-based place among queued jobs in arrival order; 0 once started or finished
}

// queueManager runs at most one PS5 job at a time.
type queueManager struct {
	mu         sync.Mutex
	perSubject int
	global     int
	queued     []*queueJob // arrival order
	running    *queueJob
	jobs       map[string]*queueJob
	finished   []string
	served     map[string]uint64
	seq        uint64
	changed    chan struct{} // closed and replaced on every change, waking Next and Acquire
	now        func() time.Time

	// picker chooses the next job from the queued jobs (arrival order, never empty) and returns its index.
	// It is called with mu held and must not call back into the queue. The scheduler may replace it.
	picker func(queued []*queueJob) int
}

// newQueueManager applies the defaults (3 per subject, 20 global) for any limit that is not positive.
func newQueueManager(perSubject, global int) *queueManager {
	if perSubject <= 0 {
		perSubject = queueDefaultPerSubject
	}
	if global <= 0 {
		global = queueDefaultGlobal
	}
	q := &queueManager{
		perSubject: perSubject,
		global:     global,
		jobs:       map[string]*queueJob{},
		served:     map[string]uint64{},
		changed:    make(chan struct{}),
		now:        time.Now,
	}
	q.picker = q.pickLeastRecentlyServed
	return q
}

// Enqueue adds a job for subject. The job is removed if ctx ends before it starts, and its context is cancelled
// if ctx ends while it runs. Enqueue returns errQueueFull when a limit is reached.
func (q *queueManager) Enqueue(ctx context.Context, subject, engine, model string) (*queueJob, error) {
	id, err := queueNewID()
	if err != nil {
		return nil, err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	mine := 0
	for _, j := range q.queued {
		if j.Subject == subject {
			mine++
		}
	}
	if mine >= q.perSubject || len(q.queued) >= q.global {
		return nil, errQueueFull
	}
	jctx, cancel := context.WithCancel(ctx)
	job := &queueJob{ID: id, Subject: subject, Engine: engine, Model: model, Enqueued: q.clock(), ctx: jctx, cancel: cancel, state: queueStateQueued}
	job.stop = context.AfterFunc(ctx, func() { q.abandon(job) })
	q.queued = append(q.queued, job)
	q.jobs[id] = job
	q.notifyLocked()
	return job, nil
}

// errQueueAbandoned is returned by Acquire when the job was removed before it started.
var errQueueAbandoned = errors.New("ps5 job abandoned before it started")

// clock returns the manager's time source, so tests can control enqueue times.
func (q *queueManager) clock() time.Time {
	if q.now != nil {
		return q.now()
	}
	return time.Now()
}

// Next blocks until nothing is running and a job is queued, then starts the job the picker chooses.
// It returns ctx.Err() if ctx ends first.
func (q *queueManager) Next(ctx context.Context) (*queueJob, error) {
	for {
		q.mu.Lock()
		if job := q.startNextLocked(); job != nil {
			q.mu.Unlock()
			return job, nil
		}
		changed := q.changed
		q.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
	}
}

// Acquire waits until job is the running job and returns nil. Any waiter starts the picker's choice when the slot
// is free, so no dispatcher goroutine is needed. If ctx ends first, the job is abandoned and the caller must still
// call Finish, because the job may already hold the slot.
func (q *queueManager) Acquire(ctx context.Context, job *queueJob) error {
	for {
		q.mu.Lock()
		q.startNextLocked()
		if q.running == job {
			q.mu.Unlock()
			return nil
		}
		if queueTerminal(job.state) {
			q.mu.Unlock()
			return errQueueAbandoned
		}
		changed := q.changed
		q.mu.Unlock()
		select {
		case <-ctx.Done():
			q.abandon(job)
			return ctx.Err()
		case <-changed:
		}
	}
}

// startNextLocked starts the picker's choice when nothing is running and returns it, or returns nil.
func (q *queueManager) startNextLocked() *queueJob {
	if q.running != nil || len(q.queued) == 0 {
		return nil
	}
	for _, j := range q.queued {
		j.servedSeq = q.served[j.Subject]
	}
	i := q.picker(q.queued)
	if i < 0 || i >= len(q.queued) {
		i = 0
	}
	job := q.queued[i]
	q.queued = slices.Delete(q.queued, i, i+1)
	q.seq++
	q.served[job.Subject] = q.seq
	q.running = job
	job.state = queueStateSwitching
	q.notifyLocked()
	return job
}

// Advance moves the running job to switching, loading or generating. It returns false for any other job or state.
func (q *queueManager) Advance(job *queueJob, state queueState) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.running != job || queueTerminal(job.state) {
		return false
	}
	if state != queueStateSwitching && state != queueStateLoading && state != queueStateGenerating {
		return false
	}
	job.state = state
	q.notifyLocked()
	return true
}

// Finish ends the running job with done, failed or cancelled and frees the PS5 slot. Call it only after the PS5
// request has returned, because the next job starts as soon as the slot is free. A job cancelled while running keeps
// its cancelled state; the slot is still released here.
func (q *queueManager) Finish(job *queueJob, state queueState) {
	if !queueTerminal(state) {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.running != job {
		return
	}
	q.running = nil
	q.retireLocked(job, state)
	q.notifyLocked()
}

// Status returns the public state of a job, or false when the ID is unknown or the job was evicted.
func (q *queueManager) Status(id string) (queueSnapshot, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	job, ok := q.jobs[id]
	if !ok {
		return queueSnapshot{}, false
	}
	s := queueSnapshot{ID: job.ID, Subject: job.Subject, Engine: job.Engine, Model: job.Model, State: job.state}
	if i := slices.Index(q.queued, job); i >= 0 {
		s.Position = i + 1
	}
	return s, true
}

// abandon runs when a job's caller context ends. A queued job is removed; a running job is marked cancelled and
// its context is cancelled, but the slot stays held until the runner calls Finish.
func (q *queueManager) abandon(job *queueJob) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if queueTerminal(job.state) {
		return
	}
	if q.running == job {
		job.state = queueStateCancelled
		job.cancel()
		q.notifyLocked()
		return
	}
	i := slices.Index(q.queued, job)
	if i < 0 {
		return
	}
	q.queued = slices.Delete(q.queued, i, i+1)
	q.retireLocked(job, queueStateCancelled)
	q.notifyLocked()
}

// pickLeastRecentlyServed chooses the queued job whose subject was served least recently (never served first),
// breaking ties by arrival order. Subjects therefore alternate, and each subject's jobs stay FIFO.
func (q *queueManager) pickLeastRecentlyServed(queued []*queueJob) int {
	best := 0
	for i, j := range queued {
		if q.served[j.Subject] < q.served[queued[best].Subject] {
			best = i
		}
	}
	return best
}

// retireLocked sets the final state, releases the job's resources and keeps it reachable until it is evicted.
func (q *queueManager) retireLocked(job *queueJob, state queueState) {
	if !queueTerminal(job.state) {
		job.state = state
	}
	job.stop()
	job.cancel()
	q.finished = append(q.finished, job.ID)
	if len(q.finished) > queueRetainFinished {
		delete(q.jobs, q.finished[0])
		q.finished = q.finished[1:]
	}
}

// notifyLocked wakes every goroutine blocked in Next.
func (q *queueManager) notifyLocked() {
	close(q.changed)
	q.changed = make(chan struct{})
}

func queueTerminal(s queueState) bool {
	return s == queueStateDone || s == queueStateFailed || s == queueStateCancelled
}

// queueNewID returns a 128-bit random job ID as 32 lowercase hex characters.
func queueNewID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
