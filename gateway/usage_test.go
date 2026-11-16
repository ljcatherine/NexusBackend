package main

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type usageTestClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *usageTestClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *usageTestClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

func usageTestReadLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	var out []map[string]any
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var m map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &m); err != nil {
			t.Fatalf("line is not JSON: %v", err)
		}
		out = append(out, m)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan %s: %v", path, err)
	}
	return out
}

func TestUsageRecordFieldsAndSubjectHash(t *testing.T) {
	dir := t.TempDir()
	clock := &usageTestClock{t: time.Date(2026, 11, 17, 12, 30, 0, 0, time.UTC)}
	log, err := newUsageLog(dir, clock.now)
	if err != nil {
		t.Fatalf("newUsageLog: %v", err)
	}

	err = log.Record("owner-subject", usageRecord{
		Route: "chat", Engine: "llm", Model: "ps5-local", Outcome: "ok",
		QueueMS: 12, RunMS: 3400, InBytes: 512, OutBytes: 2048,
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}

	lines := usageTestReadLines(t, filepath.Join(dir, "usage-2026-11.jsonl"))
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(lines))
	}
	got := lines[0]
	wantKeys := []string{"ts", "subject_hash", "route", "engine", "model", "outcome", "queue_ms", "run_ms", "in_bytes", "out_bytes"}
	if len(got) != len(wantKeys) {
		t.Fatalf("record has %d keys, want exactly %d: %v", len(got), len(wantKeys), got)
	}
	for _, k := range wantKeys {
		if _, ok := got[k]; !ok {
			t.Errorf("missing field %q", k)
		}
	}
	if got["ts"] != "2026-11-17T12:30:00Z" {
		t.Errorf("ts = %v", got["ts"])
	}
	if got["subject_hash"] != auditSubjectID("owner-subject") {
		t.Errorf("subject_hash = %v, want auditSubjectID", got["subject_hash"])
	}
	if got["route"] != "chat" || got["engine"] != "llm" || got["model"] != "ps5-local" || got["outcome"] != "ok" {
		t.Errorf("string fields wrong: %v", got)
	}
	if got["queue_ms"] != float64(12) || got["run_ms"] != float64(3400) || got["in_bytes"] != float64(512) || got["out_bytes"] != float64(2048) {
		t.Errorf("numeric fields wrong: %v", got)
	}
	if strings.Contains(string(mustReadFile(t, filepath.Join(dir, "usage-2026-11.jsonl"))), "owner-subject") {
		t.Errorf("raw subject written to usage log")
	}
}

func TestUsageMonthlyRollover(t *testing.T) {
	dir := t.TempDir()
	clock := &usageTestClock{t: time.Date(2026, 11, 30, 23, 59, 59, 0, time.UTC)}
	log, err := newUsageLog(dir, clock.now)
	if err != nil {
		t.Fatalf("newUsageLog: %v", err)
	}
	if err := log.Record("s", usageRecord{Route: "chat"}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	clock.set(time.Date(2026, 12, 1, 0, 0, 1, 0, time.UTC))
	if err := log.Record("s", usageRecord{Route: "chat"}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if n := len(usageTestReadLines(t, filepath.Join(dir, "usage-2026-11.jsonl"))); n != 1 {
		t.Errorf("november file has %d lines, want 1", n)
	}
	if n := len(usageTestReadLines(t, filepath.Join(dir, "usage-2026-12.jsonl"))); n != 1 {
		t.Errorf("december file has %d lines, want 1", n)
	}
}

func TestUsagePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not enforced on Windows")
	}
	dir := filepath.Join(t.TempDir(), "usage")
	clock := &usageTestClock{t: time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)}
	log, err := newUsageLog(dir, clock.now)
	if err != nil {
		t.Fatalf("newUsageLog: %v", err)
	}
	if err := log.Record("s", usageRecord{Route: "chat"}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Errorf("dir perm = %o, want 700", perm)
	}
	fileInfo, err := os.Stat(filepath.Join(dir, "usage-2026-11.jsonl"))
	if err != nil {
		t.Fatalf("stat file: %v", err)
	}
	if perm := fileInfo.Mode().Perm(); perm != 0o600 {
		t.Errorf("file perm = %o, want 600", perm)
	}
}

func TestUsageConcurrentAppendsStayWholeLines(t *testing.T) {
	dir := t.TempDir()
	clock := &usageTestClock{t: time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)}
	log, err := newUsageLog(dir, clock.now)
	if err != nil {
		t.Fatalf("newUsageLog: %v", err)
	}
	const writers, each = 8, 25
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < each; j++ {
				if err := log.Record("s", usageRecord{Route: "chat", InBytes: 1}); err != nil {
					t.Errorf("Record: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if n := len(usageTestReadLines(t, filepath.Join(dir, "usage-2026-11.jsonl"))); n != writers*each {
		t.Errorf("got %d lines, want %d", n, writers*each)
	}
}

func TestUsageNeverRecordsPromptOrAudio(t *testing.T) {
	const secretPrompt = "SECRET-PROMPT-7f3a9c-do-not-log"
	const secretAudio = "AUDIO-BASE64-DATA-91bd"
	body := `{"model":"ps5-local","messages":[{"role":"user","content":"` + secretPrompt + `"}],"audio":"` + secretAudio + `"}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	raw, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read request: %v", err)
	}

	dir := t.TempDir()
	clock := &usageTestClock{t: time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)}
	log, err := newUsageLog(dir, clock.now)
	if err != nil {
		t.Fatalf("newUsageLog: %v", err)
	}
	// Build the record the way a handler would: sizes and labels only, never the body.
	rec := usageRecord{
		Route: "chat", Engine: "llm", Model: "ps5-local", Outcome: "ok",
		QueueMS: 1, RunMS: 2, InBytes: int64(len(raw)), OutBytes: 0,
	}
	if err := log.Record("caller-subject", rec); err != nil {
		t.Fatalf("Record: %v", err)
	}

	content := mustReadFile(t, filepath.Join(dir, "usage-2026-11.jsonl"))
	for _, secret := range []string{secretPrompt, secretAudio, "messages", "audio"} {
		if strings.Contains(string(content), secret) {
			t.Errorf("usage log contains %q", secret)
		}
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}
