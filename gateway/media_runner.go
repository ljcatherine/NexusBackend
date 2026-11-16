package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"slices"
	"sync"
	"time"
)

// mediaEngine is the PS5 engine that serves image, video, STT, TTS and voice work.
const mediaEngine = "media"

// Finished results wait here until read once or until the TTL ends. The total stays under mediaResMaxBytes.
const (
	mediaResDefaultTTL = 10 * time.Minute
	mediaResMaxBytes   = 128 << 20
)

// mediaSwitchTimeout bounds the engine switch and the wait for media readiness.
// shortcut: readiness is found by polling /health, not by a PS5 push event; upgrade when the PS5 pushes engine state.
// mediaOfflineGrace is how long the PS5 may stay unreachable after a switch is requested, for the app restart.
var (
	mediaSwitchTimeout = ps5EngineSwitchTimeout
	mediaPollInterval  = 500 * time.Millisecond
	mediaOfflineGrace  = 45 * time.Second
)

// mediaOffline is the refusal for a PS5 that cannot be reached. Admission and the engine wait both send it.
func mediaOffline() *mediaRunFailure {
	return &mediaRunFailure{Status: http.StatusServiceUnavailable, RetryAfter: "30", Message: "PS5 is offline"}
}

// mediaPS5Offline reports the breaker's offline state, as opposed to a switching or loading state.
func mediaPS5Offline(s ps5State) bool {
	return !s.Online && s.Reason == "ps5_offline"
}

// mediaForwardRoute is the PSllama path and per-request timeout for one media kind.
type mediaForwardRoute struct {
	path    string
	timeout time.Duration
}

// mediaForwardRoutes lists the media kinds Provider.Media forwards. Voice is SSE and is not forwarded here.
var mediaForwardRoutes = map[string]mediaForwardRoute{
	"image":         {"/v1/images/generations", ps5ImageTimeout},
	"video":         {"/v1/videos/generations", ps5VideoTimeout},
	"transcription": {"/v1/audio/transcriptions", ps5STTTimeout},
	"speech":        {"/v1/audio/speech", ps5TTSTimeout},
}

// mediaForward sends one media request body, unchanged, to the PS5 media API and returns the raw response.
func (p *ps5Provider) mediaForward(ctx context.Context, kind string, req ProviderRequest) (MediaResult, error) {
	route, ok := mediaForwardRoutes[kind]
	if !ok {
		return MediaResult{}, &ps5Error{Message: "unknown media request"}
	}
	var out MediaResult
	err := p.control.post(ctx, p.control.media, route.path, route.timeout, req, func(resp *http.Response, _ string) error {
		body, err := io.ReadAll(io.LimitReader(resp.Body, ps5MaxResponseBytes+1))
		if err != nil {
			return &ps5Error{Status: resp.StatusCode, Message: "PS5 response read failed"}
		}
		if len(body) > ps5MaxResponseBytes {
			return &ps5Error{Status: resp.StatusCode, Message: "PS5 response too large"}
		}
		contentType := resp.Header.Get("Content-Type")
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		out = MediaResult{ContentType: contentType, Body: body}
		return nil
	})
	return out, err
}

// mediaJobSpec describes one media request. It holds no prompt, messages or audio.
type mediaJobSpec struct {
	Route       string // usage route, e.g. /v1/images/generations
	Permission  string // quota permission, e.g. quotaPermissionImage
	QuotaAmount int    // units charged to Permission's daily quota
	Model       string // model folder or ID, for the usage line and the scheduler
	InBytes     int64  // request body size, for the usage line
}

// mediaRunFunc performs the PS5 call once the PS5 is in media mode and the job holds its slot. It returns its
// output through mediaSetResult and never writes to a response.
type mediaRunFunc func(ctx context.Context, job *queueJob) error

// mediaRunFailure is a refusal or failure sent in place of a result. Message is fixed or sanitized.
type mediaRunFailure struct {
	Status     int
	RetryAfter string
	Message    string
}

func (f *mediaRunFailure) Error() string { return f.Message }

func (f *mediaRunFailure) write(w http.ResponseWriter) {
	if f.RetryAfter != "" {
		w.Header().Set("Retry-After", f.RetryAfter)
	}
	writeError(w, f.Status, f.Message)
}

type mediaSinkKey struct{}

// mediaSink carries a job's output from the run function back to the runner.
type mediaSink struct{ res MediaResult }

// mediaSetResult records the output of a run. It does nothing outside a media job.
func mediaSetResult(ctx context.Context, res MediaResult) {
	if s, ok := ctx.Value(mediaSinkKey{}).(*mediaSink); ok {
		s.res = res
	}
}

// mediaAdmit runs the checks before the queue: PS5 online, daily quota, then a place in the queue.
func (g *Gateway) mediaAdmit(ctx context.Context, subject string, spec mediaJobSpec) (*queueJob, *mediaRunFailure) {
	if !g.ps5Snapshot().Online {
		return nil, mediaOffline()
	}
	if _, err := g.consumeQuota(subject, spec.Permission, spec.QuotaAmount); err != nil {
		switch {
		case errors.Is(err, errQuotaExceeded):
			return nil, &mediaRunFailure{Status: http.StatusTooManyRequests, Message: "daily quota exceeded"}
		case errors.Is(err, errQuotaNotConfigured):
			return nil, &mediaRunFailure{Status: http.StatusForbidden, Message: "no daily quota is configured for this AI permission"}
		}
		return nil, &mediaRunFailure{Status: http.StatusBadRequest, Message: err.Error()}
	}
	job, err := g.queue.Enqueue(ctx, subject, mediaEngine, spec.Model)
	if errors.Is(err, errQueueFull) {
		g.quota.Refund(subject, spec.Permission, spec.QuotaAmount)
		return nil, &mediaRunFailure{Status: http.StatusTooManyRequests, RetryAfter: "10", Message: "PS5 queue is full"}
	}
	if err != nil {
		g.quota.Refund(subject, spec.Permission, spec.QuotaAmount)
		return nil, &mediaRunFailure{Status: http.StatusInternalServerError, Message: "AI queue is unavailable"}
	}
	return job, nil
}

// mediaRefusal captures beginInference's refusal, so one lifecycle gate serves both request and background jobs.
type mediaRefusal struct {
	header http.Header
	status int
	body   []byte
}

func (m *mediaRefusal) Header() http.Header {
	if m.header == nil {
		m.header = http.Header{}
	}
	return m.header
}

func (m *mediaRefusal) WriteHeader(code int) {
	if m.status == 0 {
		m.status = code
	}
}

func (m *mediaRefusal) Write(p []byte) (int, error) {
	if m.status == 0 {
		m.status = http.StatusOK
	}
	m.body = append(m.body, p...)
	return len(p), nil
}

func (m *mediaRefusal) failure() *mediaRunFailure {
	var env struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(m.body, &env)
	return &mediaRunFailure{Status: m.status, Message: env.Error.Message}
}

// reachLabels names each engine in the refusals reachEngine sends.
var reachLabels = map[string]string{mediaEngine: "media", "llm": "chat"}

// reachEngine moves the PS5 to engine and waits until that engine is ready, within mediaSwitchTimeout. It polls
// before the switch, so a PS5 that dropped while the job queued fails at once. An offline PS5 outside a switch fails
// at once too; during a switch, offline polls are allowed for mediaOfflineGrace.
func (g *Gateway) reachEngine(job *queueJob, engine string) *mediaRunFailure {
	label := reachLabels[engine]
	ctx, cancel := context.WithTimeout(job.Context(), mediaSwitchTimeout)
	defer cancel()
	s := g.ps5.health.Poll(ctx)
	if mediaPS5Offline(s) {
		return mediaOffline()
	}
	switching := s.Engine != engine
	if switching {
		if err := g.ps5.EnsureEngine(ctx, engine); err != nil {
			return &mediaRunFailure{Status: http.StatusServiceUnavailable, RetryAfter: "30", Message: "PS5 could not switch to " + label + " mode"}
		}
	}
	g.queue.Advance(job, queueStateLoading)
	var offlineSince time.Time
	for {
		s := g.ps5.health.Poll(ctx)
		if s.Online && s.Engine == engine && s.Ready {
			return nil
		}
		if mediaPS5Offline(s) {
			if !switching {
				return mediaOffline()
			}
			if offlineSince.IsZero() {
				offlineSince = time.Now()
			}
			if time.Since(offlineSince) >= mediaOfflineGrace {
				return mediaOffline()
			}
		} else {
			offlineSince = time.Time{}
		}
		select {
		case <-ctx.Done():
			return &mediaRunFailure{Status: http.StatusServiceUnavailable, RetryAfter: "30", Message: "PS5 " + label + " is not ready"}
		case <-time.After(mediaPollInterval):
		}
	}
}

// mediaFailureFromErr maps a failed run to a 502. Only the sanitized PS5 message reaches the caller.
func mediaFailureFromErr(err error) *mediaRunFailure {
	msg := "AI media request failed"
	var pe *ps5Error
	if errors.As(err, &pe) && pe.Message != "" {
		msg = pe.Message
	}
	return &mediaRunFailure{Status: http.StatusBadGateway, Message: msg}
}

// mediaExecute waits for the PS5 slot, switches to media mode, runs run and releases the slot. keep, when set,
// receives a successful result before the slot is released, so a stored result exists by the time the job reads
// as done. It returns the final queue state and, for a refusal or failure, the response to send.
func (g *Gateway) mediaExecute(waitCtx context.Context, job *queueJob, subject string, spec mediaJobSpec, run mediaRunFunc, keep func(MediaResult)) (state queueState, res MediaResult, fail *mediaRunFailure) {
	// refund returns the quota charged in mediaAdmit for an exit where the PS5 never ran the generation.
	refund := func() { g.quota.Refund(subject, spec.Permission, spec.QuotaAmount) }
	if err := g.queue.Acquire(waitCtx, job); err != nil {
		refund()
		g.queue.Finish(job, queueStateCancelled)
		return queueStateCancelled, MediaResult{}, nil
	}
	started := time.Now()
	sink := &mediaSink{}
	state = queueStateFailed
	defer func() {
		if state == queueStateDone && keep != nil {
			keep(sink.res)
		}
		g.queue.Finish(job, state)
		g.recordMediaUsage(subject, spec, string(state), started.Sub(job.Enqueued), time.Since(started), int64(len(sink.res.Body)))
	}()

	refusal := &mediaRefusal{}
	if !g.beginInference(refusal) {
		refund()
		return state, res, refusal.failure()
	}
	defer g.aiLifecycle.Unlock()
	if fail = g.reachEngine(job, mediaEngine); fail != nil {
		refund()
		if job.Context().Err() != nil {
			return queueStateCancelled, res, nil
		}
		return state, res, fail
	}
	if job.Context().Err() != nil {
		refund()
		return queueStateCancelled, res, nil
	}
	g.queue.Advance(job, queueStateGenerating)
	runCtx := context.WithValue(job.Context(), mediaSinkKey{}, sink)
	if err := run(runCtx, job); err != nil {
		if job.Context().Err() != nil {
			return queueStateCancelled, res, nil
		}
		return state, res, mediaFailureFromErr(err)
	}
	return queueStateDone, sink.res, nil
}

// recordMediaUsage appends one usage line. It has no prompt, messages, audio or output text.
func (g *Gateway) recordMediaUsage(subject string, spec mediaJobSpec, outcome string, queued, run time.Duration, out int64) {
	if g.usage == nil {
		return
	}
	if err := g.usage.Record(subject, usageRecord{Route: spec.Route, Engine: mediaEngine, Model: spec.Model,
		Outcome: outcome, QueueMS: queued.Milliseconds(), RunMS: run.Milliseconds(), InBytes: spec.InBytes, OutBytes: out}); err != nil {
		log.Printf("usage_event status=failed")
	}
}

// runMediaJob serves a media request synchronously. It returns true only when the run succeeded, and then the
// caller writes the response. On any other outcome it has already written the refusal or failure, or the client
// went away and nothing is written.
func (g *Gateway) runMediaJob(w http.ResponseWriter, r *http.Request, spec mediaJobSpec, run mediaRunFunc) bool {
	subject := principalFromRequest(r)
	job, fail := g.mediaAdmit(r.Context(), subject, spec)
	if fail != nil {
		fail.write(w)
		return false
	}
	state, _, fail := g.mediaExecute(r.Context(), job, subject, spec, run, nil)
	if fail != nil {
		fail.write(w)
	}
	return state == queueStateDone
}

// startMediaJob queues a media job and runs it in the background, tied to lifetime (the server's context) rather
// than to a request. A successful result is kept in mediaResults until it is read once or its TTL ends. Refusals at
// admission return an error; later failures show as the job's failed state. It returns the job ID.
func (g *Gateway) startMediaJob(lifetime context.Context, subject string, spec mediaJobSpec, run mediaRunFunc) (string, error) {
	job, fail := g.mediaAdmit(lifetime, subject, spec)
	if fail != nil {
		return "", fail
	}
	go g.mediaExecute(lifetime, job, subject, spec, run, func(res MediaResult) {
		if err := mediaResults.Put(job.ID, subject, res); err != nil {
			log.Printf("media_result status=not_kept")
		}
	})
	return job.ID, nil
}

var errMediaResTooLarge = errors.New("media result exceeds the result store limit")

type mediaResEntry struct {
	subject string
	result  MediaResult
	expires time.Time
}

// mediaResStore holds finished media results for one read each. Entries expire after ttl, and when the total would
// exceed maxBytes, the oldest entries are dropped first.
type mediaResStore struct {
	mu       sync.Mutex
	now      func() time.Time
	ttl      time.Duration
	maxBytes int
	total    int
	entries  map[string]mediaResEntry
	order    []string // insertion order, which is also expiry order because the TTL is fixed
}

// mediaResults is the process-wide store used by startMediaJob and read by the job result handler.
var mediaResults = newMediaResStore(time.Now, mediaResDefaultTTL, mediaResMaxBytes)

func newMediaResStore(now func() time.Time, ttl time.Duration, maxBytes int) *mediaResStore {
	return &mediaResStore{now: now, ttl: ttl, maxBytes: maxBytes, entries: map[string]mediaResEntry{}}
}

// Put keeps res under id for subject. A result larger than the whole store is refused.
func (s *mediaResStore) Put(id, subject string, res MediaResult) error {
	size := len(res.Body)
	if size > s.maxBytes {
		return errMediaResTooLarge
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeLocked()
	if _, ok := s.entries[id]; ok {
		s.removeLocked(id)
	}
	for s.total+size > s.maxBytes && len(s.order) > 0 {
		s.removeLocked(s.order[0])
	}
	s.entries[id] = mediaResEntry{subject: subject, result: res, expires: s.now().Add(s.ttl)}
	s.order = append(s.order, id)
	s.total += size
	return nil
}

// Take returns and removes the result for id, but only for its owner and before it expires. Any other call reports
// false and leaves the entry in place.
func (s *mediaResStore) Take(id, subject string) (MediaResult, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeLocked()
	e, ok := s.entries[id]
	if !ok || e.subject != subject {
		return MediaResult{}, false
	}
	s.removeLocked(id)
	return e.result, true
}

func (s *mediaResStore) purgeLocked() {
	now := s.now()
	for len(s.order) > 0 && !now.Before(s.entries[s.order[0]].expires) {
		s.removeLocked(s.order[0])
	}
}

func (s *mediaResStore) removeLocked(id string) {
	if e, ok := s.entries[id]; ok {
		s.total -= len(e.result.Body)
		delete(s.entries, id)
	}
	if i := slices.Index(s.order, id); i >= 0 {
		s.order = slices.Delete(s.order, i, i+1)
	}
}
