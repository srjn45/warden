package tui

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

func TestOneInFlightRefreshAndCoalescing(t *testing.T) {
	f := &fakeAPI{}
	m := newListPane(f, "%9", "")

	require.False(t, m.refreshInFlight)
	require.False(t, m.refreshQueued)

	// First tick starts an in-flight refresh.
	nm, cmd := m.Update(tickMsg(time.Now()))
	m = nm.(controlPaneModel)
	require.True(t, m.refreshInFlight, "first tick must set refreshInFlight")
	require.False(t, m.refreshQueued)
	require.NotNil(t, cmd)

	// Second tick while refresh is in flight must NOT launch another refresh, only coalesce (queue).
	nm, cmd2 := m.Update(tickMsg(time.Now()))
	m = nm.(controlPaneModel)
	require.True(t, m.refreshInFlight, "refreshInFlight must remain true")
	require.True(t, m.refreshQueued, "superseded tick must be coalesced into refreshQueued")
	require.NotNil(t, cmd2) // only scheduleTick, not duplicate fleet fetches

	// Third tick: still queued.
	nm, _ = m.Update(tickMsg(time.Now()))
	m = nm.(controlPaneModel)
	require.True(t, m.refreshQueued)

	// When sessionsMsg completes the in-flight refresh, the queued refresh is immediately triggered.
	nm, cmd3 := m.Update(sessionsMsg{sessions: []*store.Session{{ID: "s1"}}})
	m = nm.(controlPaneModel)
	require.True(t, m.refreshInFlight, "queued refresh must become the active in-flight refresh")
	require.False(t, m.refreshQueued, "refreshQueued must be cleared once triggered")
	require.NotNil(t, cmd3)

	// Completing the second refresh leaves no in-flight or queued refreshes.
	nm, _ = m.Update(sessionsMsg{sessions: []*store.Session{{ID: "s1"}}})
	m = nm.(controlPaneModel)
	require.False(t, m.refreshInFlight)
	require.False(t, m.refreshQueued)
}

func TestRefreshBackoffAndJitter(t *testing.T) {
	base := 1 * time.Second
	maxB := 10 * time.Second

	// Zero failures: exact base.
	require.Equal(t, base, computeRefreshBackoff(base, 0, maxB, nil))

	// Predictable jitter: jitterRand = 1.0 (max jitter).
	maxJitter := func() float64 { return 1.0 }
	// 1 failure: 1.0 * 1.5 = 1.5s, jitter = 1.5s * 0.25 = 0.375s -> 1.875s.
	b1 := computeRefreshBackoff(base, 1, maxB, maxJitter)
	require.Equal(t, 1875*time.Millisecond, b1)

	// Zero jitter: jitterRand = 0.0.
	noJitter := func() float64 { return 0.0 }
	b1NoJitter := computeRefreshBackoff(base, 1, maxB, noJitter)
	require.Equal(t, 1500*time.Millisecond, b1NoJitter)

	// Many failures: bounded by maxBackoff.
	bHigh := computeRefreshBackoff(base, 10, maxB, noJitter)
	require.Equal(t, maxB, bHigh)
}

func TestSSESnapshotUpdatesFleetAndResetsFailures(t *testing.T) {
	f := &fakeAPI{}
	m := newListPane(f, "%9", "")

	// Simulate prior failure.
	m.refreshFailures = 3
	m.fleet = fleetTimeout

	snap := []*store.Session{{ID: "agent-live", Workdir: "/work"}}
	nm, _ := m.Update(sseSnapshotMsg{sessions: snap})
	m = nm.(controlPaneModel)

	require.True(t, m.sseActive, "sseSnapshotMsg must activate sseActive")
	require.Equal(t, 0, m.refreshFailures, "successful SSE frame must reset failure count")
	require.Equal(t, fleetLive, m.fleet)
	require.False(t, m.lastCompleteAt.IsZero())
	require.Len(t, m.sessions, 1)
	require.Equal(t, "agent-live", m.sessions[0].ID)

	// Conservative tick scheduling when SSE is active.
	cmd := m.scheduleTick()
	require.NotNil(t, cmd)
}

func TestSSESnapshotErrorFallsBack(t *testing.T) {
	f := &fakeAPI{}
	m := newListPane(f, "%9", "")
	m.sseActive = true

	nm, _ := m.Update(sseSnapshotMsg{err: errors.New("connection lost")})
	m = nm.(controlPaneModel)

	require.False(t, m.sseActive, "SSE error must deactivate sseActive")
	require.Equal(t, 1, m.refreshFailures)
}

type fakeWatcherAPI struct {
	fakeAPI
	watchCalled bool
}

func (f *fakeWatcherAPI) WatchAll(ctx context.Context, onSnapshot func([]*store.Session) error) error {
	f.watchCalled = true
	return onSnapshot([]*store.Session{{ID: "watched-1"}})
}

func TestSubscribeSSECmd(t *testing.T) {
	fw := &fakeWatcherAPI{}
	ch := make(chan sseSnapshotMsg, 2)
	cmd := subscribeSSECmd(fw, ch)
	require.NotNil(t, cmd)

	msg := cmd().(sseSnapshotMsg)
	require.NoError(t, msg.err)
	require.Len(t, msg.sessions, 1)
	require.Equal(t, "watched-1", msg.sessions[0].ID)
	require.True(t, fw.watchCalled)
}
