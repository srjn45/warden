package backendusage

import (
	"time"

	"github.com/srjn45/warden/internal/backendstore"
)

// SyntheticQuotaLimit is the percent-denominated capacity used when projecting
// live UsedPercent values into BackendQuota.UsedAmount
// (UsedAmount = usedPercent/100 * SyntheticQuotaLimit). Live providers report
// percentages, not absolute token/request budgets.
const SyntheticQuotaLimit = 100.0

// QuotaWriter is the minimal backendstore write surface QuotaSync needs (D5/D6).
// *backendstore.Store satisfies it via SetQuota + SetLimited.
type QuotaWriter interface {
	SetQuota(q backendstore.BackendQuota) error
	SetLimited(backendID string, until time.Time) error
}

// SyncToStore projects a live usage Snapshot into per-scope BackendQuota records.
// One record is upserted per Limit row (keyed by Limit.Scope / window). After
// writing a backend's limits, Backend.LimitedUntil is stamped only when every
// scope for that backend is limited; it is cleared when any scope recovers (D6).
//
// Backends whose status is not ok/rate_limited (or that returned no limits) are
// skipped so transient probe failures do not wipe last-known store state.
func SyncToStore(snap Snapshot, store QuotaWriter) error {
	if store == nil {
		return nil
	}
	now := snap.GeneratedAt.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	for _, b := range snap.Backends {
		if err := syncBackend(b, store, now); err != nil {
			return err
		}
	}
	return nil
}

func syncBackend(b BackendResult, store QuotaWriter, now time.Time) error {
	if !syncableStatus(b.Status) || len(b.Usage) == 0 {
		return nil
	}

	// scope -> whether every window for that scope is limited.
	scopeLimited := map[string]bool{}
	scopeUntil := map[string]time.Time{}

	for _, lim := range b.Usage {
		q, limited, until := quotaFromLimit(b.ID, lim, now)
		if err := store.SetQuota(q); err != nil {
			return err
		}
		scope := backendstore.DefaultQuotaScope
		if lim.Scope != "" {
			scope = lim.Scope
		}
		if prev, seen := scopeLimited[scope]; !seen {
			scopeLimited[scope] = limited
		} else {
			// A scope is limited only when all of its windows are limited.
			scopeLimited[scope] = prev && limited
		}
		if limited && until.After(scopeUntil[scope]) {
			scopeUntil[scope] = until
		}
	}

	if len(scopeLimited) == 0 {
		return nil
	}
	allLimited := true
	var backendUntil time.Time
	for scope, limited := range scopeLimited {
		if !limited {
			allLimited = false
			break
		}
		if u := scopeUntil[scope]; u.After(backendUntil) {
			backendUntil = u
		}
	}
	if allLimited {
		if backendUntil.IsZero() {
			backendUntil = now.Add(5 * time.Hour)
		}
		return store.SetLimited(b.ID, backendUntil)
	}
	// Any scope recovered → clear backend-wide LimitedUntil.
	return store.SetLimited(b.ID, time.Time{})
}

func syncableStatus(s Status) bool {
	return s == StatusOK || s == StatusRateLimited
}

func quotaFromLimit(backendID string, lim Limit, now time.Time) (backendstore.BackendQuota, bool, time.Time) {
	windowType, windowDur := windowFromLimit(lim)
	used := 0.0
	if lim.UsedPercent != nil {
		used = (*lim.UsedPercent / 100.0) * SyntheticQuotaLimit
	}
	limited, until := limitCooldown(lim, now)
	q := backendstore.BackendQuota{
		BackendID:      backendID,
		Scope:          lim.Scope,
		WindowType:     windowType,
		WindowDuration: windowDur,
		QuotaLimit:     SyntheticQuotaLimit,
		UsedAmount:     used,
		LastReset:      now,
		UpdatedAt:      now,
	}
	if lim.ResetsAt != nil && !lim.ResetsAt.IsZero() {
		q.NextReset = lim.ResetsAt.UTC()
	}
	if limited {
		q.LimitedUntil = until
	}
	return q, limited, until
}

// limitIsExhausted reports whether a live Limit row is hard-limited.
// Adapters emit LimitState "reached"; the Phase 3 brief also names "rate_limited".
func limitIsExhausted(lim Limit) bool {
	if lim.LimitState != nil {
		switch *lim.LimitState {
		case "rate_limited", "reached":
			return true
		}
	}
	return lim.UsedPercent != nil && *lim.UsedPercent >= 100
}

func limitCooldown(lim Limit, now time.Time) (bool, time.Time) {
	if !limitIsExhausted(lim) {
		return false, time.Time{}
	}
	if lim.ResetsAt != nil && lim.ResetsAt.After(now) {
		return true, lim.ResetsAt.UTC()
	}
	// No reset window from the provider: fall back to the limit's own duration,
	// or a 5-hour cooldown matching the common rolling window.
	if lim.DurationMinutes != nil && *lim.DurationMinutes > 0 {
		return true, now.Add(time.Duration(*lim.DurationMinutes) * time.Minute)
	}
	return true, now.Add(5 * time.Hour)
}

func windowFromLimit(lim Limit) (backendstore.QuotaWindowType, time.Duration) {
	if lim.DurationMinutes == nil || *lim.DurationMinutes <= 0 {
		return backendstore.WindowMonthly, 30 * 24 * time.Hour
	}
	mins := *lim.DurationMinutes
	d := time.Duration(mins) * time.Minute
	switch {
	case mins <= 6*60: // ≤6h → 5-hour rolling family
		return backendstore.Window5HourRolling, d
	case mins <= 36*60: // ≤36h → daily
		return backendstore.WindowDaily, d
	case mins <= 10*24*60: // ≤10d → weekly
		return backendstore.WindowWeekly, d
	default:
		return backendstore.WindowMonthly, d
	}
}
