package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// usageRecord is one usage line. It has no field for prompts, messages or audio,
// so request content cannot reach the log through this type.
type usageRecord struct {
	TS          time.Time `json:"ts"`
	SubjectHash string    `json:"subject_hash"`
	Route       string    `json:"route"`
	Engine      string    `json:"engine"`
	Model       string    `json:"model"`
	Outcome     string    `json:"outcome"`
	QueueMS     int64     `json:"queue_ms"`
	RunMS       int64     `json:"run_ms"`
	InBytes     int64     `json:"in_bytes"`
	OutBytes    int64     `json:"out_bytes"`
}

// usageLog appends JSONL records to usage-YYYY-MM.jsonl (UTC month) under dir.
type usageLog struct {
	dir string
	now func() time.Time
	mu  sync.Mutex
}

func newUsageLog(dir string, now func() time.Time) (*usageLog, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create usage log directory: %w", err)
	}
	return &usageLog{dir: dir, now: now}, nil
}

// Record stamps the record with the current time and the hashed subject, then appends it.
func (u *usageLog) Record(subject string, rec usageRecord) error {
	rec.TS = u.now().UTC()
	rec.SubjectHash = auditSubjectID(subject)
	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encode usage record: %w", err)
	}
	line = append(line, '\n')

	u.mu.Lock()
	defer u.mu.Unlock()
	path := filepath.Join(u.dir, "usage-"+rec.TS.Format("2006-01")+".jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open usage log: %w", err)
	}
	if _, err := f.Write(line); err != nil {
		f.Close()
		return fmt.Errorf("write usage log: %w", err)
	}
	return f.Close()
}
