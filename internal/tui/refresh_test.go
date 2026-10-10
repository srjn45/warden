package tui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/client"
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
	require.Equal(t, fleetTimeout, m.fleet)
}

func TestSSESnapshotDegradedStreamErrorSetsDegradedFleet(t *testing.T) {
	f := &fakeAPI{}
	m := newListPane(f, "%9", "")
	m.sseActive = true

	nm, _ := m.Update(sseSnapshotMsg{err: &client.StreamError{Message: "session store degraded", Degraded: true}})
	m = nm.(controlPaneModel)

	require.False(t, m.sseActive, "SSE error must deactivate sseActive")
	require.Equal(t, 1, m.refreshFailures)
	require.Equal(t, fleetDegraded, m.fleet)
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

// TestSSEWatchStreamContractEndToEnd tests the typed SSE streaming contract end-to-end:
// tree and unknown named events are ignored, session frames deliver snapshots to the TUI,
// and named error frames return StreamErrors that transition TUI to degraded while retaining the fleet.
func TestSSEWatchStreamContractEndToEnd(t *testing.T) {
	frames := []string{
		// 1. tree frame (named) - must be ignored, no empty snapshot
		"event: tree\ndata: {\"projects\":[{\"id\":\"proj-1\"}]}\n\n",
		// 2. unknown named event - must be ignored
		"event: custom_unknown\ndata: {\"foo\":\"bar\"}\n\n",
		// 3. ping comment - ignored
		": ping\n\n",
		// 4. unnamed session snapshot frame - delivered
		"data: {\"sessions\":[{\"id\":\"s-live\",\"status\":\"working\",\"workdir\":\"/work\",\"tmuxSession\":\"s-live\",\"updatedAt\":\"2026-08-31T18:00:00Z\"}]}\n\n",
		// 5. named error frame with degraded metadata - returns StreamError
		"event: error\ndata: {\"error\":\"session store degraded\",\"degraded\":true}\n\n",
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		require.True(t, ok)
		for _, frame := range frames {
			_, _ = fmt.Fprint(w, frame)
			flusher.Flush()
		}
	}))
	defer ts.Close()

	cl := client.New(ts.URL)
	var snapshots [][]*store.Session

	err := cl.WatchAll(context.Background(), func(ss []*store.Session) error {
		snapshots = append(snapshots, ss)
		return nil
	})

	// WatchAll should have received the error event and returned a typed *StreamError
	require.Error(t, err)
	var se *client.StreamError
	require.True(t, errors.As(err, &se))
	require.True(t, se.Degraded)
	require.Equal(t, "session store degraded", se.Message)

	// Exactly ONE session snapshot should have been delivered (the unnamed one), not the tree frame
	require.Len(t, snapshots, 1)
	require.Len(t, snapshots[0], 1)
	require.Equal(t, "s-live", snapshots[0][0].ID)

	// Now drive the TUI model through this sequence
	m := newListPane(&fakeAPI{}, "%9", "")
	// Deliver the session snapshot
	nm, _ := m.Update(sseSnapshotMsg{sessions: snapshots[0]})
	m = nm.(controlPaneModel)
	require.Equal(t, fleetLive, m.fleet)
	require.True(t, m.sseActive)
	require.Len(t, m.sessions, 1)
	require.Equal(t, "s-live", m.sessions[0].ID)

	// Deliver the StreamError
	nm, _ = m.Update(sseSnapshotMsg{err: err})
	m = nm.(controlPaneModel)
	require.Equal(t, fleetDegraded, m.fleet)
	require.False(t, m.sseActive)
	require.Len(t, m.sessions, 1, "fleet is retained across degraded error frame")
	require.Equal(t, "s-live", m.sessions[0].ID)
}

// TestSSEInterleavedFramesKeepSelectionTerminalsAndPanes replays a hostile
// interleaving of session frames, degraded errors, transport drops, and a
// reconnect, asserting the selected agent, the opened agent pane, and the
// terminal row are never lost and nothing is spawned.
func TestSSEInterleavedFramesKeepSelectionTerminalsAndPanes(t *testing.T) {
	f := &fakeAPI{}
	m := terminalProjectPane(f, "%1")
	idx := cursorOn(m, func(it item) bool { return it.session != nil && it.session.ID == "a1" })
	require.GreaterOrEqual(t, idx, 0)
	m.cursor = idx
	m.openedAgent = "a1"

	agentFrame := func() sseSnapshotMsg {
		a := liveAgent("a1", "/alpha")
		a.ProjectID = "proj-1"
		return sseSnapshotMsg{sessions: []*store.Session{a}}
	}
	steps := []sseSnapshotMsg{
		agentFrame(),
		{err: &client.StreamError{Message: "degraded", Degraded: true}},
		{err: errors.New("unexpected EOF")},
		agentFrame(),                 // reconnect delivers a fresh agents-only frame
		{err: &client.StreamError{}}, // malformed error frame
		{err: client.ErrDaemonDown},
		agentFrame(),
	}
	for i, st := range steps {
		nm, _ := m.Update(st)
		m = nm.(controlPaneModel)
		ids := itemSessionIDs(m.items())
		require.Contains(t, ids, "a1", "step %d: agent row", i)
		require.Contains(t, ids, "t1", "step %d: terminal row", i)
		require.Equal(t, "a1", m.selectedID(), "step %d: selection", i)
		require.Equal(t, "a1", m.openedAgent, "step %d: agent pane", i)
		require.Nil(t, f.spawned, "step %d: nothing spawned", i)
	}
	require.Equal(t, fleetLive, m.fleet, "final good frame restores live health")
	require.True(t, m.sseActive)
}
