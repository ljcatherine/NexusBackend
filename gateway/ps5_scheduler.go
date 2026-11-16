package main

import "time"

// schedDefaultMaxWait is the longest a job may wait before it forces a switch
// to its engine/model, so a busy engine cannot starve other subjects.
const schedDefaultMaxWait = 120 * time.Second

// schedCandidate is one queued job as the scheduler sees it.
type schedCandidate struct {
	ID       string
	Subject  string
	Engine   string
	Model    string
	Enqueued time.Time
}

// schedPick returns the index of the next candidate to run, or -1 when there are
// none. Preference order: same engine and model, same engine, then oldest. Any
// candidate that has waited longer than maxWait (zero means the default) wins
// first, oldest of those. Ties keep slice order.
func schedPick(cands []schedCandidate, engine, model string, now time.Time, maxWait time.Duration) int {
	if maxWait <= 0 {
		maxWait = schedDefaultMaxWait
	}
	starved, exact, sameEngine, oldest := -1, -1, -1, -1
	for i, c := range cands {
		if oldest < 0 || c.Enqueued.Before(cands[oldest].Enqueued) {
			oldest = i
		}
		if now.Sub(c.Enqueued) > maxWait && (starved < 0 || c.Enqueued.Before(cands[starved].Enqueued)) {
			starved = i
		}
		if c.Engine == engine {
			if sameEngine < 0 || c.Enqueued.Before(cands[sameEngine].Enqueued) {
				sameEngine = i
			}
			if c.Model == model && (exact < 0 || c.Enqueued.Before(cands[exact].Enqueued)) {
				exact = i
			}
		}
	}
	switch {
	case starved >= 0:
		return starved
	case exact >= 0:
		return exact
	case sameEngine >= 0:
		return sameEngine
	default:
		return oldest
	}
}

// schedSwitchWarning returns the user-facing note for moving from the current
// engine/model to the target, or "" when no switch is needed. An engine switch
// restarts the PS5 server, which subsumes a model load.
func schedSwitchWarning(curEngine, curModel, engine, model string) string {
	if engine != curEngine {
		return "Switching engine restarts the PS5 server and takes about 30 s."
	}
	if model != "" && model != curModel {
		return "Loading this model takes about 100 s."
	}
	return ""
}
