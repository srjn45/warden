package autopilot

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTierStateMarkAvailableAndExpiry(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: now}
	ts := newTierState(clock.now)

	// Unmarked backend is available.
	require.True(t, ts.available("claude"))

	// Marking limited makes it unavailable until the window elapses.
	ts.markLimited("claude", now.Add(30*time.Minute))
	require.False(t, ts.available("claude"))

	// A blank backend is ignored (the daemon default has no tier identity).
	ts.markLimited("", now.Add(time.Hour))
	require.True(t, ts.available(""))

	// Still limited just before expiry; available exactly at/after it (climb-back).
	clock.t = now.Add(29 * time.Minute)
	require.False(t, ts.available("claude"))
	clock.t = now.Add(30 * time.Minute)
	require.True(t, ts.available("claude"), "expiry re-qualifies the backend")

	// A past `until` clears any existing window immediately.
	ts.markLimited("codex", now)
	require.True(t, ts.available("codex"))
}

func TestTierStateEarliestReset(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: now}
	ts := newTierState(clock.now)

	_, ok := ts.earliestReset()
	require.False(t, ok, "nothing limited ⇒ no reset")

	ts.markLimited("a", now.Add(2*time.Hour))
	ts.markLimited("b", now.Add(30*time.Minute))
	ts.markLimited("c", now.Add(time.Hour))

	earliest, ok := ts.earliestReset()
	require.True(t, ok)
	require.Equal(t, now.Add(30*time.Minute), earliest, "soonest future window wins")

	// Once the soonest window expires, the next one becomes earliest.
	clock.t = now.Add(31 * time.Minute)
	earliest, ok = ts.earliestReset()
	require.True(t, ok)
	require.Equal(t, now.Add(time.Hour), earliest)
}

// fakeClock is a movable clock for tierstate/guardian tests.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }
func (c *fakeClock) add(d time.Duration) time.Time {
	c.t = c.t.Add(d)
	return c.t
}
