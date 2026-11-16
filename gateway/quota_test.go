package main

import (
	"errors"
	"testing"
	"time"
)

func TestQuotaParsePolicy(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr error
	}{
		{name: "empty means no quotas", raw: ""},
		{name: "valid policy", raw: `{"ai.video":{"unit":"requests","limit":5},"ai.audio":{"unit":"seconds","limit":600}}`},
		{name: "zero limit allowed", raw: `{"ai.image":{"unit":"requests","limit":0}}`},
		{name: "unknown permission rejected", raw: `{"ai.chat":{"unit":"requests","limit":5}}`, wantErr: errQuotaUnknownPermission},
		{name: "negative limit rejected", raw: `{"ai.video":{"unit":"requests","limit":-1}}`, wantErr: errQuotaInvalidLimit},
		{name: "unknown unit rejected", raw: `{"ai.video":{"unit":"minutes","limit":1}}`, wantErr: errQuotaInvalidUnit},
		{name: "unknown field rejected", raw: `{"ai.video":{"unit":"requests","limit":1,"extra":true}}`, wantErr: errQuotaPolicyInvalid},
		{name: "trailing value rejected", raw: `{"ai.video":{"unit":"requests","limit":1}} {}`, wantErr: errQuotaPolicyInvalid},
		{name: "malformed JSON rejected", raw: `{"ai.video":`, wantErr: errQuotaPolicyInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseQuotaPolicy(tt.raw)
			if !errors.Is(err, tt.wantErr) && !(err == nil && tt.wantErr == nil) {
				t.Fatalf("parseQuotaPolicy error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestQuotaResetsAtUTCMidnight(t *testing.T) {
	policy, err := parseQuotaPolicy(`{"ai.video":{"unit":"requests","limit":2}}`)
	if err != nil {
		t.Fatalf("parseQuotaPolicy error = %v", err)
	}
	now := time.Date(2026, 11, 17, 23, 59, 30, 0, time.UTC)
	meter := newQuotaMeter(policy, func() time.Time { return now })

	if _, err := meter.Consume("owner", quotaPermissionVideo, 2); err != nil {
		t.Fatalf("Consume before midnight error = %v", err)
	}
	if _, err := meter.Consume("owner", quotaPermissionVideo, 1); !errors.Is(err, errQuotaExceeded) {
		t.Fatalf("Consume over limit error = %v, want errQuotaExceeded", err)
	}

	// A local zone one hour ahead of UTC must not move the reset point.
	now = time.Date(2026, 11, 18, 0, 0, 0, 0, time.FixedZone("ahead", 3600))
	if _, err := meter.Consume("owner", quotaPermissionVideo, 1); !errors.Is(err, errQuotaExceeded) {
		t.Fatalf("Consume at 2026-11-17T23:00Z error = %v, want errQuotaExceeded", err)
	}

	now = time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	remaining, err := meter.Consume("owner", quotaPermissionVideo, 1)
	if err != nil {
		t.Fatalf("Consume after UTC midnight error = %v", err)
	}
	if remaining != 1 {
		t.Fatalf("remaining after reset = %d, want 1", remaining)
	}
}

func TestQuotaPerSubjectIsolation(t *testing.T) {
	policy, _ := parseQuotaPolicy(`{"ai.audio":{"unit":"seconds","limit":10}}`)
	meter := newQuotaMeter(policy, func() time.Time { return time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC) })

	if _, err := meter.Consume("alice", quotaPermissionAudio, 10); err != nil {
		t.Fatalf("alice Consume error = %v", err)
	}
	if _, err := meter.Consume("alice", quotaPermissionAudio, 1); !errors.Is(err, errQuotaExceeded) {
		t.Fatalf("alice over-limit error = %v, want errQuotaExceeded", err)
	}
	remaining, err := meter.Consume("bob", quotaPermissionAudio, 4)
	if err != nil {
		t.Fatalf("bob Consume error = %v", err)
	}
	if remaining != 6 {
		t.Fatalf("bob remaining = %d, want 6", remaining)
	}
}

func TestQuotaRejectsUnconfiguredPermission(t *testing.T) {
	meter := newQuotaMeter(quotaPolicy{}, func() time.Time { return time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC) })
	if _, err := meter.Consume("owner", "ai.bogus", 1); !errors.Is(err, errQuotaNotConfigured) {
		t.Fatalf("Consume unknown permission error = %v, want errQuotaNotConfigured", err)
	}
	if _, err := meter.Consume("owner", quotaPermissionImage, 1); !errors.Is(err, errQuotaNotConfigured) {
		t.Fatalf("Consume unconfigured image error = %v, want errQuotaNotConfigured", err)
	}
}

func TestQuotaExceedingDoesNotDecrementBelowZero(t *testing.T) {
	policy, _ := parseQuotaPolicy(`{"ai.voice":{"unit":"requests","limit":3}}`)
	meter := newQuotaMeter(policy, func() time.Time { return time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC) })

	if remaining, err := meter.Consume("owner", quotaPermissionVoice, 2); err != nil || remaining != 1 {
		t.Fatalf("first Consume = (%d, %v), want (1, nil)", remaining, err)
	}
	if remaining, err := meter.Consume("owner", quotaPermissionVoice, 2); !errors.Is(err, errQuotaExceeded) || remaining != 1 {
		t.Fatalf("overflow Consume = (%d, %v), want (1, errQuotaExceeded)", remaining, err)
	}
	if remaining, err := meter.Consume("owner", quotaPermissionVoice, 1); err != nil || remaining != 0 {
		t.Fatalf("exact Consume = (%d, %v), want (0, nil)", remaining, err)
	}
	if remaining, err := meter.Consume("owner", quotaPermissionVoice, 1); !errors.Is(err, errQuotaExceeded) || remaining != 0 {
		t.Fatalf("exhausted Consume = (%d, %v), want (0, errQuotaExceeded)", remaining, err)
	}
	if _, err := meter.Consume("", quotaPermissionVoice, 1); !errors.Is(err, errQuotaInvalidSubject) {
		t.Fatalf("empty subject error = %v, want errQuotaInvalidSubject", err)
	}
	if _, err := meter.Consume("owner", quotaPermissionVoice, 0); !errors.Is(err, errQuotaInvalidAmount) {
		t.Fatalf("zero amount error = %v, want errQuotaInvalidAmount", err)
	}
}

func TestQuotaRefundSameDay(t *testing.T) {
	policy, _ := parseQuotaPolicy(`{"ai.voice":{"unit":"requests","limit":3}}`)
	meter := newQuotaMeter(policy, func() time.Time { return time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC) })

	if _, err := meter.Consume("owner", quotaPermissionVoice, 2); err != nil {
		t.Fatalf("Consume error = %v", err)
	}
	meter.Refund("owner", quotaPermissionVoice, 1)
	if remaining, err := meter.Consume("owner", quotaPermissionVoice, 1); err != nil || remaining != 1 {
		t.Fatalf("Consume after refund = (%d, %v), want (1, nil)", remaining, err)
	}
}

func TestQuotaRefundAfterDayRolloverIsNoop(t *testing.T) {
	policy, _ := parseQuotaPolicy(`{"ai.video":{"unit":"requests","limit":2}}`)
	now := time.Date(2026, 11, 17, 23, 59, 0, 0, time.UTC)
	meter := newQuotaMeter(policy, func() time.Time { return now })
	if _, err := meter.Consume("owner", quotaPermissionVideo, 2); err != nil {
		t.Fatalf("Consume error = %v", err)
	}

	now = time.Date(2026, 11, 18, 0, 1, 0, 0, time.UTC)
	meter.Refund("owner", quotaPermissionVideo, 2)
	// The new day starts with the full limit: the refund must not have left yesterday's usage behind or added to it.
	if remaining, err := meter.Consume("owner", quotaPermissionVideo, 2); err != nil || remaining != 0 {
		t.Fatalf("Consume on new day = (%d, %v), want (0, nil)", remaining, err)
	}
	if _, err := meter.Consume("owner", quotaPermissionVideo, 1); !errors.Is(err, errQuotaExceeded) {
		t.Fatalf("Consume over new-day limit error = %v, want errQuotaExceeded", err)
	}
}

func TestQuotaRefundNeverBelowZero(t *testing.T) {
	policy, _ := parseQuotaPolicy(`{"ai.voice":{"unit":"requests","limit":3}}`)
	meter := newQuotaMeter(policy, func() time.Time { return time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC) })

	meter.Refund("nobody", quotaPermissionVoice, 1) // unknown subject: nothing to refund, no panic
	if _, err := meter.Consume("owner", quotaPermissionVoice, 1); err != nil {
		t.Fatalf("Consume error = %v", err)
	}
	meter.Refund("owner", quotaPermissionVoice, 5)
	if remaining, err := meter.Consume("owner", quotaPermissionVoice, 3); err != nil || remaining != 0 {
		t.Fatalf("Consume after over-refund = (%d, %v), want (0, nil): usage must not go below zero", remaining, err)
	}
	if _, err := meter.Consume("owner", quotaPermissionVoice, 1); !errors.Is(err, errQuotaExceeded) {
		t.Fatalf("Consume over limit error = %v, want errQuotaExceeded", err)
	}
}
