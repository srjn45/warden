package tui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/srjn45/warden/internal/client"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// TestClassifyFleetErr pins the mapping from a List error to the fleet state that
// drives the banner: a dead daemon, a degraded store, and a timed-out/other error
// must be told apart so the operator sees the right cause.
func TestClassifyFleetErr(t *testing.T) {
	require.Equal(t, fleetLive, classifyFleetErr(nil))
	require.Equal(t, fleetDisconnected, classifyFleetErr(client.ErrDaemonDown))
	require.Equal(t, fleetDisconnected, classifyFleetErr(fmt.Errorf("wrap: %w", client.ErrDaemonDown)))
	require.Equal(t, fleetDegraded, classifyFleetErr(&client.StatusError{Code: http.StatusServiceUnavailable, Msg: "session store degraded"}))
	require.Equal(t, fleetTimeout, classifyFleetErr(context.DeadlineExceeded))
	require.Equal(t, fleetTimeout, classifyFleetErr(fmt.Errorf("wrap: %w", context.DeadlineExceeded)))
	// A reachable daemon returning a non-503 status is not "down": treat as stale.
	require.Equal(t, fleetTimeout, classifyFleetErr(&client.StatusError{Code: http.StatusInternalServerError, Msg: "boom"}))
	require.Equal(t, fleetTimeout, classifyFleetErr(errors.New("some transport error")))
}

// TestFleetBannerDetail checks the persistent, non-blocking banner text for each
// abnormal state, with and without a prior complete snapshot to cite.
func TestFleetBannerDetail(t *testing.T) {
	require.Equal(t, "", fleetBannerDetail(fleetLive, time.Now()), "live fleet shows no banner")

	at := time.Date(2026, 8, 31, 18, 4, 31, 0, time.Local)
	require.Equal(t,
		"session store degraded · showing last complete fleet from 18:04:31",
		fleetBannerDetail(fleetDegraded, at))
	require.Equal(t,
		"daemon not responding — request timed out · showing last complete fleet from 18:04:31",
		fleetBannerDetail(fleetTimeout, at))
	require.Equal(t,
		"daemon not running — start it with `warden daemon` · showing last complete fleet from 18:04:31",
		fleetBannerDetail(fleetDisconnected, at))

	// Never connected this session (zero time): no stale-from stamp; the disconnect
	// banner degrades to the plain actionable hint.
	require.Equal(t,
		"daemon not running — start it with `warden daemon`",
		fleetBannerDetail(fleetDisconnected, time.Time{}))
	require.NotContains(t, fleetBannerDetail(fleetDegraded, time.Time{}), "showing last complete fleet")
}

// threeAgents is a stable complete fleet snapshot for the retention tests.
func threeAgents() []*store.Session {
	now := time.Now()
	return []*store.Session{
		{ID: "a1", Workdir: "/w", UpdatedAt: now.Add(-1 * time.Minute)},
		{ID: "a2", Workdir: "/w", UpdatedAt: now.Add(-2 * time.Minute)},
		{ID: "a3", Workdir: "/w", UpdatedAt: now.Add(-3 * time.Minute)},
	}
}

func ids(ss []*store.Session) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = s.ID
	}
	return out
}

// TestControlPaneRetainsFleetOnDegraded is the core disappearing-row regression:
// after a complete snapshot, a degraded (503) poll must NOT clear or shrink the
// list — the last-known-good fleet stays put and only the fleet state flips.
func TestControlPaneRetainsFleetOnDegraded(t *testing.T) {
	m := newListPane(&fakeAPI{}, "%9", "")
	m = lstep(m, sessionsMsg{sessions: threeAgents()})
	require.Equal(t, fleetLive, m.fleet)
	require.False(t, m.lastCompleteAt.IsZero(), "a complete snapshot stamps the time")
	before := ids(m.sessions)

	// Move the cursor onto a specific agent so we can prove selection survives.
	m.cursor = cursorOn(m, func(it item) bool { return it.session != nil && it.session.ID == "a2" })
	require.Equal(t, "a2", m.selectedID())

	m = lstep(m, sessionsMsg{err: &client.StatusError{Code: http.StatusServiceUnavailable, Msg: "session store degraded: 1 unreadable record(s)"}})

	require.Equal(t, fleetDegraded, m.fleet)
	require.Equal(t, before, ids(m.sessions), "degraded poll must retain every row")
	require.Equal(t, "a2", m.selectedID(), "cursor/selection is preserved during degradation")
}

// TestControlPaneRetainsFleetOnDisconnect: a dead daemon retains the fleet too.
func TestControlPaneRetainsFleetOnDisconnect(t *testing.T) {
	m := newListPane(&fakeAPI{}, "%9", "")
	m = lstep(m, sessionsMsg{sessions: threeAgents()})
	before := ids(m.sessions)

	m = lstep(m, sessionsMsg{err: client.ErrDaemonDown})

	require.Equal(t, fleetDisconnected, m.fleet)
	require.Equal(t, before, ids(m.sessions), "a disconnect must not clear the fleet")
}

// TestControlPaneRetainsFleetOnTimeout: a timed-out request retains the fleet.
func TestControlPaneRetainsFleetOnTimeout(t *testing.T) {
	m := newListPane(&fakeAPI{}, "%9", "")
	m = lstep(m, sessionsMsg{sessions: threeAgents()})
	before := ids(m.sessions)

	m = lstep(m, sessionsMsg{err: context.DeadlineExceeded})

	require.Equal(t, fleetTimeout, m.fleet)
	require.Equal(t, before, ids(m.sessions), "a timeout must not clear the fleet")
}

// TestControlPaneRecoversAfterDegraded verifies recovery semantics: a later
// COMPLETE snapshot is authoritative — it clears the banner, restamps the time,
// and legitimately drops an agent it omits (an agent killed while degraded).
func TestControlPaneRecoversAfterDegraded(t *testing.T) {
	m := newListPane(&fakeAPI{}, "%9", "")
	m = lstep(m, sessionsMsg{sessions: threeAgents()})
	firstStamp := m.lastCompleteAt

	// Degrade, then confirm the fleet is retained.
	m = lstep(m, sessionsMsg{err: &client.StatusError{Code: http.StatusServiceUnavailable, Msg: "degraded"}})
	require.Equal(t, fleetDegraded, m.fleet)
	require.Len(t, m.sessions, 3)

	time.Sleep(2 * time.Millisecond) // ensure a distinct wall-clock stamp

	// Recover with a complete snapshot that omits a2 (killed during the outage).
	now := time.Now()
	m = lstep(m, sessionsMsg{sessions: []*store.Session{
		{ID: "a1", Workdir: "/w", UpdatedAt: now},
		{ID: "a3", Workdir: "/w", UpdatedAt: now.Add(-time.Minute)},
	}})

	require.Equal(t, fleetLive, m.fleet, "a complete snapshot clears the degraded state")
	require.Equal(t, []string{"a1", "a3"}, ids(m.sessions), "an authoritative snapshot drops omitted agents")
	require.True(t, m.lastCompleteAt.After(firstStamp), "recovery restamps the last-complete time")
	require.Equal(t, "", fleetBannerDetail(m.fleet, m.lastCompleteAt), "banner is cleared after recovery")
}

// TestControlPaneBannerRendersInView drives the whole View: the degraded banner is
// visible in the header while rows are retained, and it disappears after recovery.
func TestControlPaneBannerRendersInView(t *testing.T) {
	m := newListPane(&fakeAPI{}, "%9", "")
	m = lstep(m, tea.WindowSizeMsg{Width: 120, Height: 24})
	m = lstep(m, sessionsMsg{sessions: threeAgents()})
	require.NotContains(t, m.View(), "session store degraded", "no banner while live")

	m = lstep(m, sessionsMsg{err: &client.StatusError{Code: http.StatusServiceUnavailable, Msg: "degraded"}})
	view := m.View()
	require.True(t, strings.Contains(view, "session store degraded"), "degraded banner is shown in the header")
	require.Contains(t, view, "showing last complete fleet from", "banner cites the last complete snapshot")

	m = lstep(m, sessionsMsg{sessions: threeAgents()})
	require.NotContains(t, m.View(), "session store degraded", "banner clears after a complete refresh")
}
