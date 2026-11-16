package main

import (
	"net/http"
	"regexp"
	"strings"
	"time"
)

// aiJobNotFound is the body for an unknown job and for another subject's job, so IDs cannot be probed.
const aiJobNotFound = "job not found"

var aiJobIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// aiStatusDeps are the live inputs the status handlers read. U-16 wires them into the routes.
type aiStatusDeps struct {
	health  func() ps5State
	queue   *queueManager
	now     func() time.Time
	maxWait time.Duration
}

type aiJobView struct {
	ID       string     `json:"id"`
	State    queueState `json:"state"`
	Position int        `json:"position"`
}

type aiJobDetail struct {
	aiJobView
	Engine string `json:"engine"`
	Model  string `json:"model"`
}

type aiStatusResponse struct {
	Online      bool        `json:"online"`
	Engine      string      `json:"engine,omitempty"`
	Model       string      `json:"model,omitempty"`
	Ready       bool        `json:"ready"`
	Busy        bool        `json:"busy"`
	Reason      string      `json:"reason,omitempty"`
	QueueLength int         `json:"queue_length"`
	Jobs        []aiJobView `json:"jobs"`
	Warning     string      `json:"warning,omitempty"`
}

// aiSchedulerPicker adapts queued jobs to schedPick using the PS5's current engine and model. schedPick chooses the
// tier (starved, same engine and model, same engine, any). Inside a non-starved tier the subject served least recently
// wins, ties go to the oldest job, so one subject's burst cannot run ahead of the others on the same engine.
func aiSchedulerPicker(health func() ps5State, now func() time.Time, maxWait time.Duration) func([]*queueJob) int {
	return func(queued []*queueJob) int {
		h := health()
		t := now()
		cands := make([]schedCandidate, len(queued))
		for i, j := range queued {
			cands[i] = schedCandidate{ID: j.ID, Subject: j.Subject, Engine: j.Engine, Model: j.Model, Enqueued: j.Enqueued}
		}
		best := schedPick(cands, h.Engine, h.Model, t, maxWait)
		if best < 0 {
			return best
		}
		if maxWait <= 0 {
			maxWait = schedDefaultMaxWait
		}
		if t.Sub(cands[best].Enqueued) > maxWait {
			return best // starved jobs win outright, oldest first
		}
		chosen := cands[best]
		inTier := func(c schedCandidate) bool {
			switch {
			case chosen.Engine == h.Engine && chosen.Model == h.Model:
				return c.Engine == h.Engine && c.Model == h.Model
			case chosen.Engine == h.Engine:
				return c.Engine == h.Engine
			default:
				return true
			}
		}
		for k, j := range queued {
			if !inTier(cands[k]) {
				continue
			}
			if b := queued[best]; j.servedSeq < b.servedSeq || (j.servedSeq == b.servedSeq && j.Enqueued.Before(b.Enqueued)) {
				best = k
			}
		}
		return best
	}
}

// aiCallerView returns the total queue length, the caller's running and queued jobs, and the caller's oldest queued job.
func aiCallerView(q *queueManager, subject string) (int, []aiJobView, *queueJob) {
	q.mu.Lock()
	defer q.mu.Unlock()
	jobs := []aiJobView{}
	if q.running != nil && q.running.Subject == subject {
		jobs = append(jobs, aiJobView{ID: q.running.ID, State: q.running.state})
	}
	var oldest *queueJob
	for i, j := range q.queued {
		if j.Subject != subject {
			continue
		}
		if oldest == nil {
			oldest = j
		}
		jobs = append(jobs, aiJobView{ID: j.ID, State: j.state, Position: i + 1})
	}
	return len(q.queued), jobs, oldest
}

// aiStatusHandler serves GET /v1/ai/status. subject returns the authenticated caller's subject.
func aiStatusHandler(d aiStatusDeps, subject func(*http.Request) string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller := subject(r)
		if caller == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		h := d.health()
		n, jobs, oldest := aiCallerView(d.queue, caller)
		resp := aiStatusResponse{Online: h.Online, Engine: h.Engine, Model: h.Model, Ready: h.Ready, Busy: h.Busy,
			Reason: h.Reason, QueueLength: n, Jobs: jobs}
		if !h.Online {
			resp.Reason = "ps5_offline"
		} else if oldest != nil {
			resp.Warning = schedSwitchWarning(h.Engine, h.Model, oldest.Engine, oldest.Model)
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// aiJobHandler serves GET /v1/ai/jobs/{id}. The ID is read from the URL path, so it works under any mount point.
func aiJobHandler(q *queueManager, subject func(*http.Request) string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller := subject(r)
		if caller == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/v1/ai/jobs/")
		s, ok := q.Status(id)
		if !aiJobIDPattern.MatchString(id) || !ok || s.Subject != caller {
			writeError(w, http.StatusNotFound, aiJobNotFound)
			return
		}
		writeJSON(w, http.StatusOK, aiJobDetail{
			aiJobView: aiJobView{ID: s.ID, State: s.State, Position: s.Position},
			Engine:    s.Engine,
			Model:     s.Model,
		})
	}
}
