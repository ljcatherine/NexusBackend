package main

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"time"
)

const (
	quotaPermissionImage      = "ai.image"
	quotaPermissionVideo      = "ai.video"
	quotaPermissionAudio      = "ai.audio"
	quotaPermissionVoice      = "ai.voice"
	quotaPermissionVoiceClone = "ai.voice.clone"

	quotaUnitRequests = "requests"
	quotaUnitSeconds  = "seconds"
)

var (
	errQuotaPolicyInvalid     = errors.New("quota policy is not a valid JSON object")
	errQuotaUnknownPermission = errors.New("quota policy names an unknown permission")
	errQuotaInvalidLimit      = errors.New("quota limit must be a non-negative integer")
	errQuotaInvalidUnit       = errors.New("quota unit must be requests or seconds")
	errQuotaInvalidSubject    = errors.New("quota subject is required")
	errQuotaInvalidAmount     = errors.New("quota amount must be positive")
	errQuotaNotConfigured     = errors.New("no daily quota is configured for permission")
	errQuotaExceeded          = errors.New("daily quota exceeded")
)

var quotaKnownPermissions = map[string]bool{
	quotaPermissionImage:      true,
	quotaPermissionVideo:      true,
	quotaPermissionAudio:      true,
	quotaPermissionVoice:      true,
	quotaPermissionVoiceClone: true,
}

type quotaRule struct {
	Unit  string `json:"unit"`
	Limit int    `json:"limit"`
}

// quotaPolicy maps a permission to its daily limit.
type quotaPolicy map[string]quotaRule

// parseQuotaPolicy parses NEXUS_AI_QUOTAS_JSON. An empty value means no quotas.
func parseQuotaPolicy(raw string) (quotaPolicy, error) {
	if strings.TrimSpace(raw) == "" {
		return quotaPolicy{}, nil
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	var policy quotaPolicy
	if err := decoder.Decode(&policy); err != nil {
		return nil, errQuotaPolicyInvalid
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, errQuotaPolicyInvalid
	}
	for permission, rule := range policy {
		if !quotaKnownPermissions[permission] {
			return nil, errQuotaUnknownPermission
		}
		if rule.Limit < 0 {
			return nil, errQuotaInvalidLimit
		}
		if rule.Unit != quotaUnitRequests && rule.Unit != quotaUnitSeconds {
			return nil, errQuotaInvalidUnit
		}
	}
	if policy == nil {
		policy = quotaPolicy{}
	}
	return policy, nil
}

// quotaMeter counts usage per subject and permission, resetting at UTC midnight.
type quotaMeter struct {
	mu     sync.Mutex
	policy quotaPolicy
	now    func() time.Time
	day    string
	used   map[string]map[string]int
}

func newQuotaMeter(policy quotaPolicy, now func() time.Time) *quotaMeter {
	if now == nil {
		now = time.Now
	}
	return &quotaMeter{policy: policy, now: now, used: map[string]map[string]int{}}
}

// Consume adds amount to the subject's usage and returns the remaining quota.
// On overflow it returns errQuotaExceeded with the remaining quota and does not count the amount.
func (m *quotaMeter) Consume(subject, permission string, amount int) (int, error) {
	if subject == "" {
		return 0, errQuotaInvalidSubject
	}
	if amount <= 0 {
		return 0, errQuotaInvalidAmount
	}
	rule, ok := m.policy[permission]
	if !ok {
		return 0, errQuotaNotConfigured
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if day := m.now().UTC().Format("2006-01-02"); day != m.day {
		m.day = day
		m.used = map[string]map[string]int{}
	}
	perms := m.used[subject]
	if perms == nil {
		perms = map[string]int{}
		m.used[subject] = perms
	}
	used := perms[permission]
	remaining := rule.Limit - used
	if amount > remaining {
		return remaining, errQuotaExceeded
	}
	perms[permission] = used + amount
	return remaining - amount, nil
}

// Refund gives amount back to the subject's usage, for work the PS5 never ran. It does nothing once the UTC day has
// rolled over, because yesterday's usage is already gone, and it never takes usage below zero.
func (m *quotaMeter) Refund(subject, permission string, amount int) {
	if subject == "" || amount <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if day := m.now().UTC().Format("2006-01-02"); day != m.day {
		return
	}
	perms := m.used[subject]
	if perms == nil {
		return
	}
	perms[permission] = max(0, perms[permission]-amount)
}
