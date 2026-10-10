package tui

import (
	"errors"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/client"
	"github.com/srjn45/warden/internal/projectstore"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// onTerminal matches a Terminals-section row (a Kind=terminal session).
func onTerminal(it item) bool { return it.session != nil && it.session.IsTerminal() }

func liveTerminal(id, workdir string, created time.Time) *store.Session {
	return &store.Session{ID: id, Kind: store.KindTerminal, Status: store.StatusWorking, Workdir: workdir, TmuxSession: id, CreatedAt: created}
}

// applyLiveTerminalNames prefers the live pane reading (info) over the stored
// fields for a terminal's §7 name, and falls back to the stored fields when no
// live reading is present. The ordinal follows CreatedAt across all terminals.
func TestApplyLiveTerminalNamesPrefersLiveInfo(t *testing.T) {
	now := time.Now()
	first := liveTerminal("t1", "/stored/dir", now)
	second := liveTerminal("t2", "/stored/other", now.Add(time.Second))
	rows := []item{{session: second}, {session: first}}

	// No live info → stored fallback (abbreviated path, no repo/branch).
	applyLiveTerminalNames(rows, []*store.Session{first, second}, nil)
	require.Equal(t, "2. /stored/other", rows[0].termName)
	require.Equal(t, "1. /stored/dir", rows[1].termName)

	// Live info → repo:rel/ (branch) form from the polled cwd.
	info := map[string]terminalLiveInfo{"t1": {cwd: "/repo/site", repoRoot: "/repo", branch: "main"}}
	applyLiveTerminalNames(rows, []*store.Session{first, second}, info)
	require.Equal(t, "1. repo:site/ (main)", rows[1].termName)
}

// terminalProjectPane is a cockpit with one open project (proj-1 at /alpha) that
// owns the live agent a1 and the live terminal t1. Terminals are project members:
// they render inside their project, not in a separate tab.
func terminalProjectPane(f *fakeAPI, terminalPane string) controlPaneModel {
	m := newListPane(f, "%9", terminalPane)
	m.defaultTerminalReady = true
	m.projects = []projectstore.Project{{
		ID: "proj-1", Name: "Alpha", Path: "/alpha", Status: projectstore.StatusOpen,
		Agents: []string{"a1"}, Terminals: []string{"t1"},
	}}
	m.collapsed["project:proj-1"] = false
	agent := liveAgent("a1", "/alpha")
	agent.ProjectID = "proj-1"
	term := liveTerminal("t1", "/alpha/site", time.Now())
	term.ProjectID = "proj-1"
	m = lstep(m, sessionsMsg{sessions: []*store.Session{agent, term}})
	return m
}

// Terminals render inside their project next to its agents; there is no separate
// Terminals tab or top-level Terminals section.
func TestTerminalsRenderInsideProject(t *testing.T) {
	m := terminalProjectPane(&fakeAPI{}, "%1")
	ids := itemSessionIDs(m.items())
	require.Contains(t, ids, "a1")
	require.Contains(t, ids, "t1", "the terminal shows under its project")
	for _, it := range m.items() {
		require.NotEqual(t, secTerminals, it.section, "no top-level Terminals section")
	}
	projIdx := cursorOn(m, func(it item) bool { return it.projHdr != nil && it.projHdr.id == "proj-1" })
	termIdx := cursorOn(m, onTerminal)
	require.GreaterOrEqual(t, projIdx, 0)
	require.Greater(t, termIdx, projIdx, "the terminal row follows its project header")
}

// A live terminal opens in the terminal pane (recording openedTerminal) and never
// routes to the agent pane.
func TestEnterOnTerminalOpensTerminalPane(t *testing.T) {
	m := terminalProjectPane(&fakeAPI{}, "%1")
	m.cursor = cursorOn(m, onTerminal)
	require.GreaterOrEqual(t, m.cursor, 0, "a terminal row must exist")
	nm, cmd := m.Update(key("enter"))
	m = nm.(controlPaneModel)
	require.Equal(t, "t1", m.openedTerminal, "the terminal is marked opened")
	require.NotNil(t, cmd, "opening a terminal returns a respawn command")
}

// With no terminal pane (tmux-native cockpit), Enter on a terminal is a no-op with
// a helpful status rather than hijacking the agent pane.
func TestEnterOnTerminalNoPaneShowsStatus(t *testing.T) {
	m := terminalProjectPane(&fakeAPI{}, "")
	m.cursor = cursorOn(m, onTerminal)
	require.GreaterOrEqual(t, m.cursor, 0)
	nm, _ := m.Update(key("enter"))
	m = nm.(controlPaneModel)
	require.Empty(t, m.openedTerminal, "no terminal pane ⇒ nothing opened")
	require.Contains(t, m.status, "no terminal pane")
}

// `t` creates the terminal inside the project the cursor is in — the same
// targeting as `n` — stamping that project id and using the row's dir. It is a
// direct create: there is no create/focus prompt.
func TestTKeyCreatesTerminalInCursorProject(t *testing.T) {
	f := &fakeAPI{}
	m := terminalProjectPane(f, "%1")
	m.cursor = cursorOn(m, func(it item) bool { return it.projHdr != nil && it.projHdr.id == "proj-1" })
	require.GreaterOrEqual(t, m.cursor, 0)

	nm, cmd := m.Update(key("t"))
	m = nm.(controlPaneModel)
	require.Equal(t, modeNormal, m.mode, "t creates directly, no prompt")
	require.NotNil(t, cmd)
	cmd()
	require.NotNil(t, f.spawned)
	require.Equal(t, terminalKind, f.spawned.Kind, "a terminal is created by kind, not a backend")
	require.Empty(t, f.spawned.Backend, "a terminal names no backend")
	require.Equal(t, "proj-1", f.spawned.ProjectID, "the terminal joins the project under the cursor")
	require.NotEmpty(t, f.spawned.Cwd)
}

// `t` from a row nested inside a project (an agent) targets that project too.
func TestTKeyFromChildRowUsesOwningProject(t *testing.T) {
	f := &fakeAPI{}
	m := terminalProjectPane(f, "%1")
	m.cursor = cursorOn(m, func(it item) bool { return it.session != nil && it.session.ID == "a1" })
	require.GreaterOrEqual(t, m.cursor, 0)
	_, cmd := m.Update(key("t"))
	require.NotNil(t, cmd)
	cmd()
	require.NotNil(t, f.spawned)
	require.Equal(t, "proj-1", f.spawned.ProjectID)
}

// A terminal always belongs to a project: with the cursor outside any project `t`
// refuses and spawns nothing (no orphan terminal, no daemon path-match guess).
func TestTKeyOutsideProjectIsRefused(t *testing.T) {
	f := &fakeAPI{}
	m := newListPane(f, "%9", "%1")
	m.defaultTerminalReady = true
	nm, cmd := m.Update(key("t"))
	m = nm.(controlPaneModel)
	require.Nil(t, cmd)
	require.Nil(t, f.spawned)
	require.Contains(t, m.status, "select a project")
}

// `t` is unavailable when there is no terminal pane (tmux-native cockpit).
func TestTKeyNoPaneIsRejected(t *testing.T) {
	m := newListPane(&fakeAPI{}, "%9", "")
	m = lstep(m, key("t"))
	require.Equal(t, modeNormal, m.mode, "t does nothing without a terminal pane")
	require.Contains(t, m.status, "terminal pane")
}

// Terminals are on-demand only: no session list — empty, agents-only, or after
// every terminal died — may spawn one.
func TestReconcileNeverSpawnsTerminals(t *testing.T) {
	f := &fakeAPI{}
	m := newListPane(f, "%9", "%1")
	agentsOnly := sessionsMsg{sessions: []*store.Session{{ID: "a1", Workdir: "/w"}}}
	for i := 0; i < 5; i++ {
		nm, cmd := m.Update(agentsOnly)
		m = nm.(controlPaneModel)
		if cmd != nil {
			cmd()
		}
	}
	require.Nil(t, f.spawned, "an agents-only listing must not auto-spawn a terminal")

	// A live terminal that then disappears is not replaced either.
	m = lstep(m, sessionsMsg{sessions: []*store.Session{liveTerminal("t1", "/w", time.Now())}})
	m.openedTerminal = "t1"
	nm, cmd := m.Update(agentsOnly)
	m = nm.(controlPaneModel)
	require.Empty(t, m.openedTerminal, "stale openedTerminal is cleared")
	if cmd != nil {
		cmd()
	}
	require.Nil(t, f.spawned, "losing every terminal must not spawn a replacement")
}

// The daemon's SSE fleet frame is agents-only. Regression for the runaway-spawn
// loop: terminals from the last complete list must survive an SSE frame, and a
// frame without them must not spawn one.
func TestSSEFrameKeepsTerminalsAndNeverSpawns(t *testing.T) {
	f := &fakeAPI{}
	m := terminalProjectPane(f, "%1")
	require.Contains(t, itemSessionIDs(m.items()), "t1")

	frame := sseSnapshotMsg{sessions: []*store.Session{func() *store.Session {
		a := liveAgent("a1", "/alpha")
		a.ProjectID = "proj-1"
		return a
	}()}}
	for i := 0; i < 5; i++ {
		nm, cmd := m.Update(frame)
		m = nm.(controlPaneModel)
		if cmd != nil {
			// reconcile / SSE re-arm commands may run; none may spawn.
			_ = cmd
		}
	}
	require.Contains(t, itemSessionIDs(m.items()), "t1", "the terminal survives agents-only SSE frames")
	require.Nil(t, f.spawned, "an agents-only SSE frame must not spawn a terminal")
}

// TestSSEErrorOrDropRetainsTerminalsAndNeverRunsFalseReconciliation: when an SSE
// stream encounters an error or drops, the existing fleet and terminals are retained,
// the opened terminal is not wiped out, and no false pane reconciliation is run.
func TestSSEErrorOrDropRetainsTerminalsAndNeverRunsFalseReconciliation(t *testing.T) {
	f := &fakeAPI{}
	m := terminalProjectPane(f, "%1")
	m.openedTerminal = "t1"
	require.Contains(t, itemSessionIDs(m.items()), "t1")
	require.Equal(t, "t1", m.openedTerminal)

	// Degraded stream error
	nm, cmd := m.Update(sseSnapshotMsg{err: &client.StreamError{Message: "store degraded", Degraded: true}})
	m = nm.(controlPaneModel)
	require.Equal(t, fleetDegraded, m.fleet)
	require.False(t, m.sseActive)
	require.Equal(t, "t1", m.openedTerminal, "opened terminal must be preserved across SSE error")
	require.Contains(t, itemSessionIDs(m.items()), "t1")
	require.Contains(t, itemSessionIDs(m.items()), "a1")
	require.Nil(t, f.spawned)
	if cmd != nil {
		_ = cmd
	}

	// Transport drop
	nm, _ = m.Update(sseSnapshotMsg{err: errors.New("connection reset by peer")})
	m = nm.(controlPaneModel)
	require.Equal(t, fleetTimeout, m.fleet)
	require.False(t, m.sseActive)
	require.Equal(t, "t1", m.openedTerminal, "opened terminal must be preserved across stream drop")
	require.Contains(t, itemSessionIDs(m.items()), "t1")
	require.Contains(t, itemSessionIDs(m.items()), "a1")
	require.Nil(t, f.spawned)

	// Disconnect
	nm, _ = m.Update(sseSnapshotMsg{err: client.ErrDaemonDown})
	m = nm.(controlPaneModel)
	require.Equal(t, fleetDisconnected, m.fleet)
	require.False(t, m.sseActive)
	require.Equal(t, "t1", m.openedTerminal, "opened terminal must be preserved across disconnect")
	require.Contains(t, itemSessionIDs(m.items()), "t1")
	require.Contains(t, itemSessionIDs(m.items()), "a1")
	require.Nil(t, f.spawned)
}

// withTerminalsFrom carries previous terminals across a frame without mutating
// the fresh slice, and lets a streamed terminal win over the carried copy.
func TestWithTerminalsFrom(t *testing.T) {
	a := liveAgent("a1", "/w")
	oldT := liveTerminal("t1", "/old", time.Now())
	newT := liveTerminal("t1", "/new", time.Now())
	fresh := []*store.Session{a}
	out := withTerminalsFrom(fresh, []*store.Session{liveAgent("gone", "/g"), oldT})
	require.Len(t, fresh, 1, "fresh is not mutated")
	require.Equal(t, []string{"a1", "t1"}, []string{out[0].ID, out[1].ID})

	out = withTerminalsFrom([]*store.Session{a, newT}, []*store.Session{oldT})
	require.Len(t, out, 2)
	require.Equal(t, "/new", out[1].Workdir, "a streamed terminal wins over the carried one")
}

// When the terminal pane is dead but a live terminal exists, reconcile re-attaches.
func TestReconcileReattachesDeadPane(t *testing.T) {
	old := isTmuxPaneDead
	isTmuxPaneDead = func(string) bool { return true }
	t.Cleanup(func() { isTmuxPaneDead = old })

	f := &fakeAPI{}
	m := newListPane(f, "%9", "%1")
	m.defaultTerminalReady = true
	m.openedTerminal = "t1"

	nm, cmd := m.Update(sessionsMsg{sessions: []*store.Session{liveTerminal("t1", "/w", time.Now())}})
	m = nm.(controlPaneModel)
	require.NotNil(t, cmd, "a dead terminal pane must be re-opened")
	require.Nil(t, f.spawned, "re-attach must not spawn a new terminal")
}

// When a live terminal already exists at startup, it is adopted into the terminal
// pane rather than spawning a new one.
func TestAdoptsExistingTerminalAtStartup(t *testing.T) {
	f := &fakeAPI{}
	m := newListPane(f, "%9", "%1")
	nm, cmd := m.Update(sessionsMsg{sessions: []*store.Session{liveTerminal("t1", "/w", time.Now())}})
	m = nm.(controlPaneModel)
	require.True(t, m.defaultTerminalReady)
	require.Equal(t, "t1", m.openedTerminal, "the existing terminal is adopted")
	require.NotNil(t, cmd, "adoption opens it in the pane")
	cmd()
	require.Nil(t, f.spawned, "an existing terminal is not re-spawned")
}

// Opening an agent records its dir (used by the agent pane and rotation).
func TestOpenAgentRecordsDir(t *testing.T) {
	f := &fakeAPI{}
	m := newListPane(f, "%9", "%1")
	m.defaultTerminalReady = true
	m = lstep(m, sessionsMsg{sessions: []*store.Session{{ID: "a1", Status: store.StatusWorking, Workdir: "/agent/w", TmuxSession: "a1"}}})
	m.cursor = cursorOn(m, func(it item) bool { return it.session != nil && it.session.ID == "a1" })
	require.GreaterOrEqual(t, m.cursor, 0)
	nm, cmd := m.Update(key("enter"))
	m = nm.(controlPaneModel)
	require.NotNil(t, cmd)
	require.Equal(t, "/agent/w", m.openedAgentDir)
}

// terminalSpawnedMsg records the new terminal and (with a pane) opens it.
func TestTerminalSpawnedMsgOpens(t *testing.T) {
	m := newListPane(&fakeAPI{}, "%9", "%1")
	nm, cmd := m.Update(terminalSpawnedMsg{id: "t-new", focus: true})
	m = nm.(controlPaneModel)
	require.Equal(t, "t-new", m.openedTerminal)
	require.NotNil(t, cmd, "a spawned terminal is opened + list/projects refreshed")
}
