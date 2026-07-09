package autopilot

import (
	"sync"
	"time"
)

// tierState records per-backend rate-limit windows for cost-tier backend
// selection (autopilot.md §7). A backend marked limited-until a future instant is
// unavailable; the entry expires at that instant, at which point the backend
// re-qualifies and naturally "climbs back up" the ladder on the next selection.
// It is fed by the poller's existing rate-limit detection (the reset-time parse
// when available, else the configured retry/spend fallbacks — the daemon computes
// that instant and passes it to markLimited). Safe for concurrent use.
type tierState struct {
	mu    sync.Mutex
	now   func() time.Time
	until map[string]time.Time // backend → limited-until (absent ⇒ never limited)
}

// newTierState builds a tierState reading the current time through now (injected
// so tests drive expiry with a fake clock). A nil now defaults to time.Now.
func newTierState(now func() time.Time) *tierState {
	if now == nil {
		now = time.Now
	}
	return &tierState{now: now, until: map[string]time.Time{}}
}

// markLimited records that backend is rate-limited until `until`. A blank backend
// is ignored (the daemon default carries no tier identity). An `until` at or
// before now clears the entry, so a backend re-qualifies immediately.
func (ts *tierState) markLimited(backend string, until time.Time) {
	if backend == "" {
		return
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if !until.After(ts.now()) {
		delete(ts.until, backend)
		return
	}
	ts.until[backend] = until
}

// available reports whether backend is currently selectable: no live limit window
// covers it. A backend whose window has expired is available again.
func (ts *tierState) available(backend string) bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	u, ok := ts.until[backend]
	if !ok {
		return true
	}
	if !u.After(ts.now()) {
		delete(ts.until, backend) // expired — reclaim the entry
		return true
	}
	return false
}

// earliestReset returns the soonest future limit expiry across all currently
// limited backends, or ok=false when nothing is limited. The guardian uses it as
// the backoff floor so it wakes as soon as a backend frees rather than sleeping a
// full capped-exponential interval past a known reset (climb-back, §7).
func (ts *tierState) earliestReset() (time.Time, bool) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	now := ts.now()
	var earliest time.Time
	found := false
	for b, u := range ts.until {
		if !u.After(now) {
			delete(ts.until, b) // expired — reclaim on the way past
			continue
		}
		if !found || u.Before(earliest) {
			earliest, found = u, true
		}
	}
	return earliest, found
}
