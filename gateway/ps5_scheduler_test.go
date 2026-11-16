package main

import (
	"strings"
	"testing"
	"time"
)

func TestSchedulerPickPrefersCurrentEngineAndModel(t *testing.T) {
	now := time.Unix(1000, 0)
	cands := []schedCandidate{
		{ID: "a", Subject: "s1", Engine: "media", Model: "wan", Enqueued: now.Add(-10 * time.Second)},
		{ID: "b", Subject: "s2", Engine: "llm", Model: "qwen", Enqueued: now.Add(-5 * time.Second)},
		{ID: "c", Subject: "s3", Engine: "llm", Model: "gemma", Enqueued: now.Add(-1 * time.Second)},
	}
	if got := schedPick(cands, "llm", "gemma", now, 0); got != 2 {
		t.Fatalf("exact engine+model match: got index %d, want 2", got)
	}
	if got := schedPick(cands, "llm", "qwen", now, 0); got != 1 {
		t.Fatalf("exact match on qwen: got index %d, want 1", got)
	}
}

func TestSchedulerPickFallsBackToEngineThenOldest(t *testing.T) {
	now := time.Unix(1000, 0)
	cands := []schedCandidate{
		{ID: "a", Engine: "media", Model: "wan", Enqueued: now.Add(-3 * time.Second)},
		{ID: "b", Engine: "llm", Model: "qwen", Enqueued: now.Add(-2 * time.Second)},
		{ID: "c", Engine: "llm", Model: "gemma", Enqueued: now.Add(-1 * time.Second)},
	}
	if got := schedPick(cands, "llm", "other", now, 0); got != 1 {
		t.Fatalf("same engine, no model match: got index %d, want oldest same-engine (1)", got)
	}
	if got := schedPick(cands, "video", "x", now, 0); got != 0 {
		t.Fatalf("no engine match: got index %d, want oldest overall (0)", got)
	}
	if got := schedPick(nil, "llm", "qwen", now, 0); got != -1 {
		t.Fatalf("empty queue: got index %d, want -1", got)
	}
}

func TestSchedulerPickStarvationCapWins(t *testing.T) {
	now := time.Unix(1000, 0)
	cands := []schedCandidate{
		{ID: "old-media", Engine: "media", Model: "wan", Enqueued: now.Add(-150 * time.Second)},
		{ID: "older-media", Engine: "media", Model: "wan", Enqueued: now.Add(-200 * time.Second)},
		{ID: "fresh-llm", Engine: "llm", Model: "qwen", Enqueued: now.Add(-1 * time.Second)},
	}
	if got := schedPick(cands, "llm", "qwen", now, 0); got != 1 {
		t.Fatalf("starved candidates: got index %d, want oldest starved (1)", got)
	}
	// Exactly at maxWait is not yet starved.
	atLimit := []schedCandidate{
		{ID: "edge", Engine: "media", Enqueued: now.Add(-schedDefaultMaxWait)},
		{ID: "match", Engine: "llm", Model: "qwen", Enqueued: now},
	}
	if got := schedPick(atLimit, "llm", "qwen", now, 0); got != 1 {
		t.Fatalf("at maxWait: got index %d, want current match (1)", got)
	}
	// A custom maxWait is honored.
	if got := schedPick(cands[2:], "llm", "qwen", now, time.Millisecond); got != 0 {
		t.Fatalf("single candidate: got index %d, want 0", got)
	}
	custom := []schedCandidate{
		{ID: "media", Engine: "media", Enqueued: now.Add(-5 * time.Second)},
		{ID: "match", Engine: "llm", Model: "qwen", Enqueued: now},
	}
	if got := schedPick(custom, "llm", "qwen", now, 2*time.Second); got != 0 {
		t.Fatalf("custom maxWait 2s: got index %d, want starved media (0)", got)
	}
}

func TestSchedulerSwitchWarning(t *testing.T) {
	if w := schedSwitchWarning("llm", "qwen", "media", "wan"); !strings.Contains(w, "30 s") {
		t.Fatalf("engine switch warning = %q, want restart about 30 s", w)
	}
	if w := schedSwitchWarning("llm", "qwen", "llm", "gemma"); !strings.Contains(w, "100 s") {
		t.Fatalf("model load warning = %q, want about 100 s", w)
	}
	if w := schedSwitchWarning("llm", "qwen", "llm", "qwen"); w != "" {
		t.Fatalf("no switch: warning = %q, want empty", w)
	}
	if w := schedSwitchWarning("llm", "qwen", "llm", ""); w != "" {
		t.Fatalf("engine-only target: warning = %q, want empty", w)
	}
}
