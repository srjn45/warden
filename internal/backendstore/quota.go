package backendstore

import (
	"encoding/json"
	"errors"
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
	// WindowDaily represents a daily reset window (standard for Antigravity).
	WindowDaily QuotaWindowType = "daily"
	// WindowMonthly represents a monthly reset window (standard for Cursor fast requests).
	WindowMonthly QuotaWindowType = "monthly"
	// WindowRateLimit represents a rate-limit / cooldown-driven quota window.
	WindowRateLimit QuotaWindowType = "rate_limit"
)

// Valid reports whether the window type is a recognized QuotaWindowType.
func (w QuotaWindowType) Valid() bool {
	return w == Window5HourRolling || w == WindowDaily || w == WindowMonthly || w == WindowRateLimit
}

// UsageEvent represents a single recorded usage occurrence (e.g. prompt/turn tokens).
type UsageEvent struct {
	Timestamp time.Time `json:"timestamp"`
	Amount    float64   `json:"amount"`
	Model     string    `json:"model,omitempty"`
}

// BackendQuota holds the quota configuration, usage window, and rate-limit tracking for a backend.
type BackendQuota struct {
	BackendID      string          `json:"backend_id"`
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

// DefaultQuotas returns the standard default quota profiles for known backends.
func DefaultQuotas() []BackendQuota {
	now := time.Now().UTC()
	return []BackendQuota{
		{
			BackendID:      "claude",
			WindowType:     Window5HourRolling,
			WindowDuration: 5 * time.Hour,
			QuotaLimit:     500000,
			LastReset:      now,
			UpdatedAt:      now,
		},
		{
			BackendID:      "antigravity",
			WindowType:     WindowDaily,
			WindowDuration: 24 * time.Hour,
			QuotaLimit:     1000000,
			LastReset:      now,
			UpdatedAt:      now,
		},
		{
			BackendID:      "cursor",
			WindowType:     WindowMonthly,
			WindowDuration: 30 * 24 * time.Hour,
			QuotaLimit:     500,
			LastReset:      now,
			UpdatedAt:      now,
		},
		{
			BackendID:      "codex",
			WindowType:     Window5HourRolling,
			WindowDuration: 5 * time.Hour,
			QuotaLimit:     500000,
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

// GetQuota returns the quota tracking record for backendID, or ErrNotFound.
func (s *Store) GetQuota(backendID string) (BackendQuota, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getQuota(backendID)
}

func (s *Store) getQuota(backendID string) (BackendQuota, error) {
	if backendID == "" {
		return BackendQuota{}, ErrNotFound
	}
	r, err := s.quotasCol.GetByKey(backendID)
	if errors.Is(err, engine.ErrKeyNotFound) {
		return BackendQuota{}, ErrNotFound
	}
	if err != nil {
		return BackendQuota{}, err
	}
	return quotaFromRecord(r.Data)
}

// SetQuota inserts or updates a quota record for a backend.
func (s *Store) SetQuota(q BackendQuota) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.upsertQuota(q)
}

func (s *Store) upsertQuota(q BackendQuota) error {
	if q.BackendID == "" {
		return errors.New("backend ID cannot be empty")
	}
	rec, err := toRecord(q)
	if err != nil {
		return err
	}
	_, err = s.quotasCol.GetByKey(q.BackendID)
	if errors.Is(err, engine.ErrKeyNotFound) {
		if _, _, err := s.quotasCol.InsertWithKey(q.BackendID, rec); err != nil {
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
	_, err = s.quotasCol.UpdateByKey(q.BackendID, rec)
	return err
}

// ListQuotas returns all backend quota records sorted by BackendID.
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
		return out[i].BackendID < out[j].BackendID
	})
	return out, nil
}

// RecordQuotaUsage adds usage to a backend's quota tracking window.
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

	q, err := s.getQuota(backendID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			q = BackendQuota{
				BackendID:      backendID,
				WindowType:     Window5HourRolling,
				WindowDuration: 5 * time.Hour,
				QuotaLimit:     500000,
				LastReset:      ts,
				UpdatedAt:      ts,
			}
			if backendID == "antigravity" {
				q.WindowType = WindowDaily
				q.WindowDuration = 24 * time.Hour
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
	case WindowDaily:
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

// GetHeadroom calculates the headroom, current usage, limit, and limited state for a backend at the specified time.
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

	b, err := s.get(backendID)
	isLimited := false
	if err == nil {
		if b.LimitedUntil.After(now) {
			isLimited = true
		}
	}

	q, err := s.getQuota(backendID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			if isLimited {
				return 0.0, 0.0, 0.0, true, nil
			}
			return 1.0, 0.0, 0.0, false, nil
		}
		return 0, 0, 0, false, err
	}

	if q.LimitedUntil.After(now) {
		isLimited = true
	}

	CalculateQuotaUsage(&q, now)
	headroom := CalculateHeadroom(q.UsedAmount, q.QuotaLimit, isLimited)
	return headroom, q.UsedAmount, q.QuotaLimit, isLimited, nil
}

// SetBackendLimited sets the LimitedUntil cooldown timestamp on both the backend row and its quota record.
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

	q, err := s.getQuota(backendID)
	if err == nil {
		q.LimitedUntil = until
		q.UpdatedAt = time.Now().UTC()
		_ = s.upsertQuota(q)
	}
	return nil
}

// ResetQuota resets a backend's quota usage, clears events, and clears any rate limits.
func (s *Store) ResetQuota(backendID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resetQuota(backendID)
}

func (s *Store) resetQuota(backendID string) error {
	now := time.Now().UTC()
	q, err := s.getQuota(backendID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	q.UsedAmount = 0
	q.Events = nil
	q.LastReset = now
	q.LimitedUntil = time.Time{}
	q.UpdatedAt = now
	if err := s.upsertQuota(q); err != nil {
		return err
	}

	b, err := s.get(backendID)
	if err == nil {
		b.LimitedUntil = time.Time{}
		return s.upsert(b)
	}
	return nil
}

// SetQuotaLimit updates the quota capacity, window type, and window duration for a backend.
func (s *Store) SetQuotaLimit(backendID string, limit float64, windowType QuotaWindowType, duration time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.setQuotaLimit(backendID, limit, windowType, duration)
}

func (s *Store) setQuotaLimit(backendID string, limit float64, windowType QuotaWindowType, duration time.Duration) error {
	if backendID == "" {
		return errors.New("backend ID cannot be empty")
	}
	now := time.Now().UTC()
	q, err := s.getQuota(backendID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			q = BackendQuota{
				BackendID: backendID,
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
