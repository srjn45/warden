package backendstore

import (
	"encoding/json"
	"errors"
	"math"
	"sort"
	"time"

	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/scriva/query"
)

// QuotaWindowType represents the type of quota rolling/reset window.
type QuotaWindowType string

const (
	// Window5HourRolling represents a 5-hour rolling usage window (standard for Claude).
	Window5HourRolling QuotaWindowType = "5h_rolling"
	// WindowDaily represents a daily reset window.
	WindowDaily QuotaWindowType = "daily"
	// WindowWeekly represents a weekly reset window (Antigravity weekly buckets).
	WindowWeekly QuotaWindowType = "weekly"
	// WindowMonthly represents a monthly reset window (standard for Cursor fast requests).
	WindowMonthly QuotaWindowType = "monthly"
	// WindowRateLimit represents a rate-limit / cooldown-driven quota window.
	WindowRateLimit QuotaWindowType = "rate_limit"

	// DefaultQuotaScope is the scope used when BackendQuota.Scope or
	// ModelEntry.QuotaScope is blank (legacy / custom rows).
	DefaultQuotaScope = "default"
)

// Valid reports whether the window type is a recognized QuotaWindowType.
func (w QuotaWindowType) Valid() bool {
	return w == Window5HourRolling || w == WindowDaily || w == WindowWeekly || w == WindowMonthly || w == WindowRateLimit
}

// UsageEvent represents a single recorded usage occurrence (e.g. prompt/turn tokens).
type UsageEvent struct {
	Timestamp time.Time `json:"timestamp"`
	Amount    float64   `json:"amount"`
	Model     string    `json:"model,omitempty"`
}

// BackendQuota holds the quota configuration, usage window, and rate-limit tracking for a backend scope.
type BackendQuota struct {
	BackendID      string          `json:"backend_id"`
	Scope          string          `json:"scope,omitempty"` // canonical scope; blank migrates to "default"
	WindowType     QuotaWindowType `json:"window_type"`
	WindowDuration time.Duration   `json:"window_duration"`
	QuotaLimit     float64         `json:"quota_limit"` // total quota in tokens, requests, or turns
	UsedAmount     float64         `json:"used_amount"`
	LastReset      time.Time       `json:"last_reset"`
	NextReset      time.Time       `json:"next_reset,omitempty"`
	LimitedUntil   time.Time       `json:"limited_until,omitempty"`
	Events         []UsageEvent    `json:"events,omitempty"`
	UpdatedAt      time.Time       `json:"updated_at"`
}

// quotaKey returns the ScrivaDB key for a (backend, scope) quota record.
func quotaKey(backendID, scope string) string {
	return backendID + ":" + normalizeQuotaScope(scope)
}

func normalizeQuotaScope(scope string) string {
	if scope == "" {
		return DefaultQuotaScope
	}
	return scope
}

// quotaStorageKey returns the ScrivaDB key for q. Single-window scopes use
// backend:scope. A weekly window that shares a scope with a 5h window (Antigravity)
// is stored under backend:scope:weekly so both coexist; GetModelHeadroom mins them.
func quotaStorageKey(q BackendQuota) string {
	base := quotaKey(q.BackendID, q.Scope)
	if q.WindowType == WindowWeekly {
		return base + ":" + string(WindowWeekly)
	}
	return base
}

// DefaultQuotas returns the standard per-scope default quota profiles for known backends
// (docs/specs/2026-09-26-per-scope-quota-routing.md D7).
func DefaultQuotas() []BackendQuota {
	now := time.Now().UTC()
	weekly := 7 * 24 * time.Hour
	monthly := 30 * 24 * time.Hour
	return []BackendQuota{
		{
			BackendID:      "claude",
			Scope:          "session",
			WindowType:     Window5HourRolling,
			WindowDuration: 5 * time.Hour,
			QuotaLimit:     500000,
			LastReset:      now,
			UpdatedAt:      now,
		},
		{
			BackendID:      "codex",
			Scope:          "codex",
			WindowType:     Window5HourRolling,
			WindowDuration: 5 * time.Hour,
			QuotaLimit:     500000,
			LastReset:      now,
			UpdatedAt:      now,
		},
		{
			BackendID:      "antigravity",
			Scope:          "gemini",
			WindowType:     Window5HourRolling,
			WindowDuration: 5 * time.Hour,
			QuotaLimit:     1000000,
			LastReset:      now,
			UpdatedAt:      now,
		},
		{
			BackendID:      "antigravity",
			Scope:          "gemini",
			WindowType:     WindowWeekly,
			WindowDuration: weekly,
			QuotaLimit:     1000000,
			LastReset:      now,
			UpdatedAt:      now,
		},
		{
			BackendID:      "antigravity",
			Scope:          "non-gemini",
			WindowType:     Window5HourRolling,
			WindowDuration: 5 * time.Hour,
			QuotaLimit:     1000000,
			LastReset:      now,
			UpdatedAt:      now,
		},
		{
			BackendID:      "antigravity",
			Scope:          "non-gemini",
			WindowType:     WindowWeekly,
			WindowDuration: weekly,
			QuotaLimit:     1000000,
			LastReset:      now,
			UpdatedAt:      now,
		},
		{
			BackendID:      "cursor",
			Scope:          "api",
			WindowType:     WindowMonthly,
			WindowDuration: monthly,
			QuotaLimit:     500,
			LastReset:      now,
			UpdatedAt:      now,
		},
		{
			BackendID:      "cursor",
			Scope:          "auto",
			WindowType:     WindowMonthly,
			WindowDuration: monthly,
			QuotaLimit:     500,
			LastReset:      now,
			UpdatedAt:      now,
		},
		{
			BackendID:      "cursor",
			Scope:          "included",
			WindowType:     WindowMonthly,
			WindowDuration: monthly,
			QuotaLimit:     500,
			LastReset:      now,
			UpdatedAt:      now,
		},
	}
}

// CalculateQuotaUsage computes the current active usage and next reset timestamp based on the quota window type.
func CalculateQuotaUsage(q *BackendQuota, now time.Time) {
	if q == nil {
		return
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}

	switch q.WindowType {
	case Window5HourRolling:
		window := q.WindowDuration
		if window <= 0 {
			window = 5 * time.Hour
		}
		cutoff := now.Add(-window)
		var activeEvents []UsageEvent
		var total float64
		for _, e := range q.Events {
			if !e.Timestamp.Before(cutoff) {
				activeEvents = append(activeEvents, e)
				total += e.Amount
			}
		}
		q.Events = activeEvents
		q.UsedAmount = total
		if len(activeEvents) > 0 {
			q.NextReset = activeEvents[0].Timestamp.Add(window)
		} else {
			q.NextReset = now.Add(window)
		}

	case WindowDaily:
		if q.LastReset.IsZero() {
			q.LastReset = now
			q.UsedAmount = 0
			q.Events = nil
		} else if now.Year() != q.LastReset.Year() || now.YearDay() != q.LastReset.YearDay() {
			q.LastReset = now
			q.UsedAmount = 0
			q.Events = nil
		}
		q.NextReset = time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)

	case WindowWeekly:
		if q.LastReset.IsZero() {
			q.LastReset = now
			q.UsedAmount = 0
			q.Events = nil
		} else {
			y1, w1 := q.LastReset.ISOWeek()
			y2, w2 := now.ISOWeek()
			if y1 != y2 || w1 != w2 {
				q.LastReset = now
				q.UsedAmount = 0
				q.Events = nil
			}
		}
		// Next Monday 00:00 UTC.
		weekday := int(now.Weekday())
		if weekday == 0 {
			weekday = 7
		}
		daysUntilMonday := 8 - weekday
		if daysUntilMonday == 7 {
			daysUntilMonday = 0
		}
		next := time.Date(now.Year(), now.Month(), now.Day()+daysUntilMonday, 0, 0, 0, 0, time.UTC)
		if !next.After(now) {
			next = next.AddDate(0, 0, 7)
		}
		q.NextReset = next

	case WindowMonthly:
		if q.LastReset.IsZero() {
			q.LastReset = now
			q.UsedAmount = 0
			q.Events = nil
		} else if now.Year() != q.LastReset.Year() || now.Month() != q.LastReset.Month() {
			q.LastReset = now
			q.UsedAmount = 0
			q.Events = nil
		}
		q.NextReset = time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, time.UTC)

	case WindowRateLimit:
		if q.LimitedUntil.After(now) {
			q.UsedAmount = q.QuotaLimit
			q.NextReset = q.LimitedUntil
		} else {
			q.UsedAmount = 0
			q.NextReset = time.Time{}
		}
	}
}

// CalculateHeadroom computes the remaining headroom fraction [0.0, 1.0].
func CalculateHeadroom(used, limit float64, isLimited bool) float64 {
	if isLimited {
		return 0.0
	}
	if limit <= 0 {
		return 1.0
	}
	ratio := used / limit
	if ratio >= 1.0 {
		return 0.0
	}
	if ratio <= 0.0 {
		return 1.0
	}
	return 1.0 - ratio
}

func quotaFromRecord(d map[string]any) (BackendQuota, error) {
	b, err := json.Marshal(d)
	if err != nil {
		return BackendQuota{}, err
	}
	var out BackendQuota
	if err := json.Unmarshal(b, &out); err != nil {
		return BackendQuota{}, err
	}
	return out, nil
}

// migrateQuotaScopeIfNeeded rewrites a legacy scope-less record (key = backendID)
// to Scope="default" under key backendID:default. Caller holds s.mu.
func (s *Store) migrateQuotaScopeIfNeeded(backendID string) error {
	if backendID == "" {
		return nil
	}
	r, err := s.quotasCol.GetByKey(backendID)
	if errors.Is(err, engine.ErrKeyNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	q, err := quotaFromRecord(r.Data)
	if err != nil {
		return err
	}
	if q.Scope != "" {
		// Already scoped but still under the legacy key — move to scoped key.
		return s.rewriteQuotaKey(backendID, q)
	}
	q.Scope = DefaultQuotaScope
	q.BackendID = backendID
	if err := s.upsertQuota(q); err != nil {
		return err
	}
	_ = s.quotasCol.DeleteByKey(backendID)
	return nil
}

// rewriteQuotaKey moves q from legacyKey to its scoped storage key.
func (s *Store) rewriteQuotaKey(legacyKey string, q BackendQuota) error {
	if q.Scope == "" {
		q.Scope = DefaultQuotaScope
	}
	if err := s.upsertQuota(q); err != nil {
		return err
	}
	newKey := quotaStorageKey(q)
	if newKey != legacyKey {
		_ = s.quotasCol.DeleteByKey(legacyKey)
	}
	return nil
}

// GetQuota returns a quota tracking record for backendID. When scope is empty it
// returns the first record for the backend (after migrating any legacy key).
// When scope is set it returns the primary (non-weekly) window for that scope,
// falling back to any matching window.
func (s *Store) GetQuota(backendID string, scope ...string) (BackendQuota, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sc := ""
	if len(scope) > 0 {
		sc = scope[0]
	}
	return s.getQuota(backendID, sc)
}

func (s *Store) getQuota(backendID, scope string) (BackendQuota, error) {
	if backendID == "" {
		return BackendQuota{}, ErrNotFound
	}
	if err := s.migrateQuotaScopeIfNeeded(backendID); err != nil {
		return BackendQuota{}, err
	}

	if scope != "" {
		scope = normalizeQuotaScope(scope)
		// Prefer the primary (non-weekly) storage key.
		if r, err := s.quotasCol.GetByKey(quotaKey(backendID, scope)); err == nil {
			return quotaFromRecord(r.Data)
		} else if err != nil && !errors.Is(err, engine.ErrKeyNotFound) {
			return BackendQuota{}, err
		}
		// Fall back to any window for this scope (e.g. weekly-only).
		all, err := s.listQuotasFor(backendID, scope)
		if err != nil {
			return BackendQuota{}, err
		}
		if len(all) == 0 {
			return BackendQuota{}, ErrNotFound
		}
		return all[0], nil
	}

	// No scope: any record for this backend.
	all, err := s.listQuotasFor(backendID, "")
	if err != nil {
		return BackendQuota{}, err
	}
	if len(all) == 0 {
		return BackendQuota{}, ErrNotFound
	}
	return all[0], nil
}

// SetQuota inserts or updates a quota record. Blank Scope migrates to "default".
func (s *Store) SetQuota(q BackendQuota) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.migrateQuotaScopeIfNeeded(q.BackendID); err != nil {
		return err
	}
	return s.upsertQuota(q)
}

func (s *Store) upsertQuota(q BackendQuota) error {
	if q.BackendID == "" {
		return errors.New("backend ID cannot be empty")
	}
	if q.Scope == "" {
		q.Scope = DefaultQuotaScope
	}
	key := quotaStorageKey(q)
	rec, err := toRecord(q)
	if err != nil {
		return err
	}
	_, err = s.quotasCol.GetByKey(key)
	if errors.Is(err, engine.ErrKeyNotFound) {
		if _, _, err := s.quotasCol.InsertWithKey(key, rec); err != nil {
			if errors.Is(err, engine.ErrDuplicateKey) {
				return ErrExists
			}
			return err
		}
		return nil
	}
	if err != nil {
		return err
	}
	_, err = s.quotasCol.UpdateByKey(key, rec)
	return err
}

// ListQuotas returns all backend quota records sorted by BackendID, then Scope, then WindowType.
func (s *Store) ListQuotas() ([]BackendQuota, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listQuotas()
}

func (s *Store) listQuotas() ([]BackendQuota, error) {
	results, err := s.quotasCol.Scan(query.MatchAll)
	if err != nil {
		return nil, err
	}
	out := make([]BackendQuota, 0, len(results))
	for _, r := range results {
		q, err := quotaFromRecord(r.Data)
		if err != nil {
			continue
		}
		out = append(out, q)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].BackendID != out[j].BackendID {
			return out[i].BackendID < out[j].BackendID
		}
		if out[i].Scope != out[j].Scope {
			return out[i].Scope < out[j].Scope
		}
		return out[i].WindowType < out[j].WindowType
	})
	return out, nil
}

// listQuotasFor returns every quota record for backendID. Caller holds s.mu.
func (s *Store) listQuotasFor(backendID, _ string) ([]BackendQuota, error) {
	all, err := s.listQuotas()
	if err != nil {
		return nil, err
	}
	out := make([]BackendQuota, 0, len(all))
	for _, q := range all {
		if q.BackendID != backendID {
			continue
		}
		out = append(out, q)
	}
	return out, nil
}

// listQuotasForScope returns every window record for (backendID, scope). Caller holds s.mu.
func (s *Store) listQuotasForScope(backendID, scope string) ([]BackendQuota, error) {
	scope = normalizeQuotaScope(scope)
	all, err := s.listQuotas()
	if err != nil {
		return nil, err
	}
	out := make([]BackendQuota, 0, 2)
	for _, q := range all {
		if q.BackendID != backendID {
			continue
		}
		if normalizeQuotaScope(q.Scope) != scope {
			continue
		}
		out = append(out, q)
	}
	return out, nil
}

// RecordQuotaUsage adds usage to a backend's quota tracking window for the model's scope.
func (s *Store) RecordQuotaUsage(backendID string, amount float64, model string, ts time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recordQuotaUsage(backendID, amount, model, ts)
}

func (s *Store) recordQuotaUsage(backendID string, amount float64, model string, ts time.Time) error {
	if backendID == "" {
		return errors.New("backend ID cannot be empty")
	}
	if amount <= 0 {
		return nil
	}
	if ts.IsZero() {
		ts = time.Now().UTC()
	} else {
		ts = ts.UTC()
	}
	if err := s.migrateQuotaScopeIfNeeded(backendID); err != nil {
		return err
	}

	scope := DefaultQuotaScope
	if model != "" {
		if m, err := s.getModel(backendID, model); err == nil {
			scope = normalizeQuotaScope(m.QuotaScope)
		} else {
			// Model unknown: prefer an existing non-default scope if the backend
			// has exactly one scope, otherwise default.
			if scopes := s.scopesForBackend(backendID); len(scopes) == 1 {
				scope = scopes[0]
			}
		}
	}

	q, err := s.getQuota(backendID, scope)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			q = BackendQuota{
				BackendID:      backendID,
				Scope:          scope,
				WindowType:     Window5HourRolling,
				WindowDuration: 5 * time.Hour,
				QuotaLimit:     500000,
				LastReset:      ts,
				UpdatedAt:      ts,
			}
			if backendID == "antigravity" {
				q.WindowType = Window5HourRolling
				q.WindowDuration = 5 * time.Hour
				q.QuotaLimit = 1000000
			} else if backendID == "cursor" {
				q.WindowType = WindowMonthly
				q.WindowDuration = 30 * 24 * time.Hour
				q.QuotaLimit = 500
			}
		} else {
			return err
		}
	}

	switch q.WindowType {
	case Window5HourRolling:
		q.Events = append(q.Events, UsageEvent{
			Timestamp: ts,
			Amount:    amount,
			Model:     model,
		})
		CalculateQuotaUsage(&q, ts)
		if len(q.Events) > 2000 {
			q.Events = q.Events[len(q.Events)-2000:]
		}
	case WindowDaily, WindowWeekly:
		CalculateQuotaUsage(&q, ts)
		q.UsedAmount += amount
		q.Events = append(q.Events, UsageEvent{
			Timestamp: ts,
			Amount:    amount,
			Model:     model,
		})
		if len(q.Events) > 1000 {
			q.Events = q.Events[len(q.Events)-1000:]
		}
	case WindowMonthly:
		CalculateQuotaUsage(&q, ts)
		q.UsedAmount += amount
		q.Events = append(q.Events, UsageEvent{
			Timestamp: ts,
			Amount:    amount,
			Model:     model,
		})
		if len(q.Events) > 1000 {
			q.Events = q.Events[len(q.Events)-1000:]
		}
	case WindowRateLimit:
		q.UsedAmount += amount
	}

	q.UpdatedAt = ts
	return s.upsertQuota(q)
}

func (s *Store) scopesForBackend(backendID string) []string {
	all, err := s.listQuotas()
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, q := range all {
		if q.BackendID != backendID {
			continue
		}
		sc := normalizeQuotaScope(q.Scope)
		if !seen[sc] {
			seen[sc] = true
			out = append(out, sc)
		}
	}
	return out
}

// GetHeadroom calculates the min headroom across all scopes for a backend at now.
func (s *Store) GetHeadroom(backendID string, now time.Time) (headroom float64, used float64, limit float64, limited bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getHeadroom(backendID, now)
}

func (s *Store) getHeadroom(backendID string, now time.Time) (float64, float64, float64, bool, error) {
	if backendID == "" {
		return 0, 0, 0, false, errors.New("backend ID cannot be empty")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	if err := s.migrateQuotaScopeIfNeeded(backendID); err != nil {
		return 0, 0, 0, false, err
	}

	backendLimited := false
	if b, err := s.get(backendID); err == nil && b.LimitedUntil.After(now) {
		backendLimited = true
	}

	all, err := s.listQuotasFor(backendID, "")
	if err != nil {
		return 0, 0, 0, false, err
	}
	if len(all) == 0 {
		if backendLimited {
			return 0.0, 0.0, 0.0, true, nil
		}
		return 1.0, 0.0, 0.0, false, nil
	}

	return minHeadroomAcross(all, backendLimited, now)
}

// GetModelHeadroom returns headroom for the model's QuotaScope (blank → "default").
// When multiple window records share that scope, returns the minimum headroom.
func (s *Store) GetModelHeadroom(backendID, modelID string, now time.Time) (headroom float64, used float64, limit float64, limited bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getModelHeadroom(backendID, modelID, now)
}

func (s *Store) getModelHeadroom(backendID, modelID string, now time.Time) (float64, float64, float64, bool, error) {
	if backendID == "" {
		return 0, 0, 0, false, errors.New("backend ID cannot be empty")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	if err := s.migrateQuotaScopeIfNeeded(backendID); err != nil {
		return 0, 0, 0, false, err
	}

	scope := DefaultQuotaScope
	if modelID != "" {
		if m, err := s.getModel(backendID, modelID); err == nil {
			scope = normalizeQuotaScope(m.QuotaScope)
		}
	}

	backendLimited := false
	if b, err := s.get(backendID); err == nil && b.LimitedUntil.After(now) {
		backendLimited = true
	}

	windows, err := s.listQuotasForScope(backendID, scope)
	if err != nil {
		return 0, 0, 0, false, err
	}
	if len(windows) == 0 {
		if backendLimited {
			return 0.0, 0.0, 0.0, true, nil
		}
		return 1.0, 0.0, 0.0, false, nil
	}
	return minHeadroomAcross(windows, backendLimited, now)
}

func minHeadroomAcross(quotas []BackendQuota, backendLimited bool, now time.Time) (float64, float64, float64, bool, error) {
	minHR := math.Inf(1)
	var usedAtMin, limitAtMin float64
	anyLimited := backendLimited

	for _, q := range quotas {
		isLimited := backendLimited
		if q.LimitedUntil.After(now) {
			isLimited = true
			anyLimited = true
		}
		CalculateQuotaUsage(&q, now)
		hr := CalculateHeadroom(q.UsedAmount, q.QuotaLimit, isLimited)
		if hr < minHR {
			minHR = hr
			usedAtMin = q.UsedAmount
			limitAtMin = q.QuotaLimit
		}
	}
	if math.IsInf(minHR, 1) {
		minHR = 1.0
	}
	return minHR, usedAtMin, limitAtMin, anyLimited, nil
}

// SetBackendLimited sets the LimitedUntil cooldown timestamp on both the backend row and its quota records.
func (s *Store) SetBackendLimited(backendID string, until time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.setBackendLimited(backendID, until)
}

func (s *Store) setBackendLimited(backendID string, until time.Time) error {
	b, err := s.get(backendID)
	if err == nil && !b.IsLocal {
		b.LimitedUntil = until
		_ = s.upsert(b)
	}

	if err := s.migrateQuotaScopeIfNeeded(backendID); err != nil {
		return err
	}
	all, err := s.listQuotasFor(backendID, "")
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, q := range all {
		q.LimitedUntil = until
		q.UpdatedAt = now
		_ = s.upsertQuota(q)
	}
	return nil
}

// ResetQuota resets a backend's quota usage across all scopes, clears events, and clears any rate limits.
func (s *Store) ResetQuota(backendID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resetQuota(backendID)
}

func (s *Store) resetQuota(backendID string) error {
	now := time.Now().UTC()
	if err := s.migrateQuotaScopeIfNeeded(backendID); err != nil {
		return err
	}
	all, err := s.listQuotasFor(backendID, "")
	if err != nil {
		return err
	}
	for _, q := range all {
		q.UsedAmount = 0
		q.Events = nil
		q.LastReset = now
		q.LimitedUntil = time.Time{}
		q.UpdatedAt = now
		if err := s.upsertQuota(q); err != nil {
			return err
		}
	}

	b, err := s.get(backendID)
	if err == nil {
		b.LimitedUntil = time.Time{}
		return s.upsert(b)
	}
	return nil
}

// SetQuotaLimit updates the quota capacity, window type, and window duration for a backend scope.
// Blank scope targets "default".
func (s *Store) SetQuotaLimit(backendID string, limit float64, windowType QuotaWindowType, duration time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.setQuotaLimit(backendID, limit, windowType, duration)
}

func (s *Store) setQuotaLimit(backendID string, limit float64, windowType QuotaWindowType, duration time.Duration) error {
	if backendID == "" {
		return errors.New("backend ID cannot be empty")
	}
	if err := s.migrateQuotaScopeIfNeeded(backendID); err != nil {
		return err
	}
	now := time.Now().UTC()
	q, err := s.getQuota(backendID, DefaultQuotaScope)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			q = BackendQuota{
				BackendID: backendID,
				Scope:     DefaultQuotaScope,
				LastReset: now,
				UpdatedAt: now,
			}
		} else {
			return err
		}
	}
	q.QuotaLimit = limit
	if windowType.Valid() {
		q.WindowType = windowType
	}
	if duration > 0 {
		q.WindowDuration = duration
	}
	q.UpdatedAt = now
	return s.upsertQuota(q)
}
