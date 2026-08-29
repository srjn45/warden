package tui

import (
	"testing"
	"time"

	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// onTerminal matches a Terminals-section row (a Kind=terminal session).
func onTerminal(it item) bool { return it.session != nil && it.session.IsTerminal() }

func liveTerminal(id, workdir string, created time.Time) *store.Session {
	return &store.Session{ID: id, Kind: store.KindTerminal, Status: store.StatusWorking, Workdir: workdir, TmuxSession: id, CreatedAt: created}
}

// terminalItems prefers the live pane reading (info) over the stored fields for
// a terminal's §7 name, and falls back to the stored fields when no live reading
// is present.
func TestTerminalItemsPrefersLiveInfo(t *testing.T) {
	term := liveTerminal("t1", "/stored/dir", time.Now())
	// No live info → stored fallback (abbreviated path, no repo/branch).
	rows := terminalItems([]*store.Session{term}, nil)
	require.Len(t, rows, 1)
	require.Equal(t, "1. /stored/dir", rows[0].termName)
	// Live info → repo:rel/ (branch) form from the polled cwd.
	info := map[string]terminalLiveInfo{"t1": {cwd: "/repo/site", repoRoot: "/repo", branch: "main"}}
	rows = terminalItems([]*store.Session{term}, info)
	require.Equal(t, "1. repo:site/ (main)", rows[0].termName)
}

// A live terminal opens in the terminal pane (recording openedTerminal) and never
// routes to the agent pane.
func TestEnterOnTerminalOpensTerminalPane(t *testing.T) {
	f := &fakeAPI{}
	m := newListPane(f, "%9", "%1")
	m.defaultTerminalReady = true // suppress the startup ensure so it doesn't spawn
	m.currentTab = tabTerminals   // terminal rows live on the Terminals tab (§3 Phase 3)
	m = lstep(m, sessionsMsg{sessions: []*store.Session{liveTerminal("t1", "/w", time.Now())}})
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
	f := &fakeAPI{}
	m := newListPane(f, "%9", "") // no terminal pane
	m.defaultTerminalReady = true
	m.currentTab = tabTerminals // terminal rows live on the Terminals tab (§3 Phase 3)
	m = lstep(m, sessionsMsg{sessions: []*store.Session{liveTerminal("t1", "/w", time.Now())}})
	m.cursor = cursorOn(m, onTerminal)
	nm, _ := m.Update(key("enter"))
	m = nm.(controlPaneModel)
	require.Empty(t, m.openedTerminal, "no terminal pane ⇒ nothing opened")
	require.Contains(t, m.status, "no terminal pane")
}

// `t` opens the create/focus choice in the opened agent's dir; c spawns a terminal
// there with the terminal backend.
func TestTKeyCreateSpawnsTerminalInOpenedDir(t *testing.T) {
	f := &fakeAPI{}
	m := newListPane(f, "%9", "%1")
	m.defaultTerminalReady = true
	m.openedAgentDir = "/opened/agent/dir"
	m = lstep(m, key("t"))
	require.Equal(t, modeTerminalChoice, m.mode)
	require.Equal(t, "/opened/agent/dir", m.termChoiceDir)
	nm, cmd := m.Update(key("c"))
	m = nm.(controlPaneModel)
	require.Equal(t, modeNormal, m.mode)
	require.NotNil(t, cmd)
	cmd()
	require.NotNil(t, f.spawned)
	require.Equal(t, terminalKind, f.spawned.Kind, "a terminal is created by kind, not a backend")
	require.Empty(t, f.spawned.Backend, "a terminal names no backend")
	require.Equal(t, "/opened/agent/dir", f.spawned.Cwd)
}

// `t` with no opened agent falls back to $HOME as the terminal's dir.
func TestTKeyFallsBackToHomeDir(t *testing.T) {
	m := newListPane(&fakeAPI{}, "%9", "%1")
	m.defaultTerminalReady = true
	m = lstep(m, key("t"))
	require.Equal(t, modeTerminalChoice, m.mode)
	require.Equal(t, homeDir(), m.termChoiceDir)
}

// `t` is unavailable when there is no terminal pane (tmux-native cockpit).
func TestTKeyNoPaneIsRejected(t *testing.T) {
	m := newListPane(&fakeAPI{}, "%9", "")
	m = lstep(m, key("t"))
	require.Equal(t, modeNormal, m.mode, "t does nothing without a terminal pane")
	require.Contains(t, m.status, "terminal pane")
}

// In the choice prompt, f focuses an existing live terminal in that dir instead of
// spawning a new one.
func TestTerminalChoiceFocusesExisting(t *testing.T) {
	f := &fakeAPI{}
	m := newListPane(f, "%9", "%1")
	m.defaultTerminalReady = true
	m = lstep(m, sessionsMsg{sessions: []*store.Session{liveTerminal("t1", "/opened/dir", time.Now())}})
	m.openedAgentDir = "/opened/dir"
	m = lstep(m, key("t"))
	require.Equal(t, modeTerminalChoice, m.mode)
	nm, cmd := m.Update(key("f"))
	m = nm.(controlPaneModel)
	require.Equal(t, "t1", m.openedTerminal, "focus opens the existing terminal in that dir")
	require.Nil(t, f.spawned, "focus must not spawn a new terminal")
	require.NotNil(t, cmd)
}

// The startup "ensure ≥1 terminal" spawns a default terminal when none exist, once.
func TestEnsureDefaultTerminalSpawnsWhenNone(t *testing.T) {
	f := &fakeAPI{}
	m := newListPane(f, "%9", "%1")
	nm, cmd := m.Update(sessionsMsg{sessions: []*store.Session{{ID: "a1", Workdir: "/w"}}}) // only an agent
	m = nm.(controlPaneModel)
	require.True(t, m.defaultTerminalReady, "the ensure step runs once")
	require.NotNil(t, cmd, "with no terminal, a default one is spawned")
	cmd()
	require.NotNil(t, f.spawned)
	require.Equal(t, terminalKind, f.spawned.Kind, "a terminal is created by kind, not a backend")
	// A second session list must not spawn again.
	f.spawned = nil
	nm, cmd = m.Update(sessionsMsg{sessions: []*store.Session{{ID: "a1", Workdir: "/w"}}})
	m = nm.(controlPaneModel)
	require.Nil(t, cmd, "the default terminal is ensured only once")
}

// When a live terminal already exists at startup, it is adopted into the terminal
// pane rather than spawning a new one.
func TestEnsureDefaultTerminalAdoptsExisting(t *testing.T) {
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

// The tmux-native cockpit (no terminal pane) never spawns a default terminal.
func TestEnsureDefaultTerminalSkippedWithoutPane(t *testing.T) {
	f := &fakeAPI{}
	m := newListPane(f, "%9", "")
	nm, cmd := m.Update(sessionsMsg{sessions: []*store.Session{{ID: "a1", Workdir: "/w"}}})
	m = nm.(controlPaneModel)
	require.Nil(t, cmd, "no terminal pane ⇒ no default terminal")
	require.False(t, m.defaultTerminalReady)
}

// Opening an agent records its dir so `t` can target it (§6.1).
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
	require.NotNil(t, cmd, "a spawned terminal is opened + list refreshed")
}
