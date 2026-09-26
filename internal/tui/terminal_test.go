package tui

import (
	"fmt"
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
	// A second session list must not spawn again while the first spawn is pending.
	f.spawned = nil
	nm, cmd = m.Update(sessionsMsg{sessions: []*store.Session{{ID: "a1", Workdir: "/w"}}})
	m = nm.(controlPaneModel)
	require.Nil(t, cmd, "a pending default-terminal spawn must not fire twice")
}

// When every live terminal disappears, reconcile spawns a replacement (§11).
func TestReconcileSpawnsWhenAllTerminalsDie(t *testing.T) {
	f := &fakeAPI{}
	m := newListPane(f, "%9", "%1")
	m = lstep(m, sessionsMsg{sessions: []*store.Session{liveTerminal("t1", "/w", time.Now())}})
	m.defaultTerminalReady = true
	m.openedTerminal = "t1"

	nm, cmd := m.Update(sessionsMsg{sessions: []*store.Session{{ID: "a1", Workdir: "/w"}}})
	m = nm.(controlPaneModel)
	require.NotNil(t, cmd, "no live terminals ⇒ spawn a default replacement")
	require.Empty(t, m.openedTerminal, "stale openedTerminal is cleared")
	cmd()
	require.NotNil(t, f.spawned)
	require.Equal(t, terminalKind, f.spawned.Kind)
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

// emptyAgentList is a sessionsMsg with only a non-terminal agent — the fixture
// that previously triggered unbounded default-terminal auto-spawns (#465).
func emptyAgentList() sessionsMsg {
	return sessionsMsg{sessions: []*store.Session{{ID: "a1", Workdir: "/w"}}}
}

// driveEmptyReconcile runs one reconcile cycle: sessionsMsg (empty terminals)
// then, if a spawn cmd was returned, completes it with a failed spawn so
// pending clears — simulating the daemon/listing race that caused the loop.
func driveEmptyReconcile(t *testing.T, m controlPaneModel, f *fakeAPI) (controlPaneModel, bool) {
	t.Helper()
	return driveEmptyReconcileWith(t, m, f, terminalSpawnedMsg{err: fmt.Errorf("spawn failed")})
}

// driveEmptyReconcileSuccess is the #465 listing-race path: Spawn returns an id
// successfully, but the subsequent session list still has zero live terminals
// (stale/slow daemon). The success callback must not reset the attempt budget.
func driveEmptyReconcileSuccess(t *testing.T, m controlPaneModel, f *fakeAPI, id string) (controlPaneModel, bool) {
	t.Helper()
	return driveEmptyReconcileWith(t, m, f, terminalSpawnedMsg{id: id, focus: false})
}

func driveEmptyReconcileWith(t *testing.T, m controlPaneModel, f *fakeAPI, done terminalSpawnedMsg) (controlPaneModel, bool) {
	t.Helper()
	attemptsBefore := m.terminalSpawnAttempts
	nm, cmd := m.Update(emptyAgentList())
	m = nm.(controlPaneModel)
	if cmd == nil {
		return m, false
	}
	cmd() // invoke spawn against fakeAPI
	require.NotNil(t, f.spawned, "reconcile returned a cmd that should spawn")
	f.spawned = nil
	nm, _ = m.Update(done)
	m = nm.(controlPaneModel)
	require.False(t, m.terminalSpawnPending, "spawn callback clears the in-flight guard")
	// Spawn callbacks (success or error) must never reset the attempt budget —
	// only a confirmed live terminal in m.sessions may (#465).
	require.Greater(t, m.terminalSpawnAttempts, attemptsBefore,
		"firing a spawn must increment attempts; callback must not reset them")
	return m, true
}

// #465: empty terminal listings must not trigger unbounded rapid auto-spawns.
// After the first spawn, subsequent empty polls within the backoff window are
// no-ops; after max attempts the circuit breaker trips and suspends auto-spawn.
func TestReconcileTerminalAutoSpawnBackoffAndCircuitBreaker(t *testing.T) {
	base := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	now := base
	old := terminalSpawnNow
	terminalSpawnNow = func() time.Time { return now }
	t.Cleanup(func() { terminalSpawnNow = old })

	f := &fakeAPI{}
	m := newListPane(f, "%9", "%1")

	// Attempt 1: immediate spawn on first empty listing.
	var spawned bool
	m, spawned = driveEmptyReconcile(t, m, f)
	require.True(t, spawned, "first empty listing must auto-spawn")
	require.Equal(t, 1, m.terminalSpawnAttempts)
	require.Equal(t, terminalSpawnInitialBackoff, m.terminalSpawnBackoff)
	require.False(t, m.terminalSpawnCircuitOpen)

	// Immediate re-poll still inside the 2s backoff → no spawn.
	m, spawned = driveEmptyReconcile(t, m, f)
	require.False(t, spawned, "must not spawn again inside the backoff window")
	require.Equal(t, 1, m.terminalSpawnAttempts)

	// Advance past backoff → attempt 2.
	now = base.Add(terminalSpawnInitialBackoff)
	m, spawned = driveEmptyReconcile(t, m, f)
	require.True(t, spawned, "second attempt after backoff")
	require.Equal(t, 2, m.terminalSpawnAttempts)
	require.Equal(t, 2*terminalSpawnInitialBackoff, m.terminalSpawnBackoff)

	// Still inside the doubled backoff → no spawn.
	m, spawned = driveEmptyReconcile(t, m, f)
	require.False(t, spawned)

	// Advance past 4s backoff → attempt 3.
	now = base.Add(terminalSpawnInitialBackoff + 2*terminalSpawnInitialBackoff)
	m, spawned = driveEmptyReconcile(t, m, f)
	require.True(t, spawned, "third attempt after doubled backoff")
	require.Equal(t, 3, m.terminalSpawnAttempts)
	require.False(t, m.terminalSpawnCircuitOpen, "breaker trips on the next empty observe")

	// Next empty listing after max attempts → circuit opens, no further spawn.
	now = base.Add(time.Hour)
	m, spawned = driveEmptyReconcile(t, m, f)
	require.False(t, spawned, "circuit breaker must suspend auto-spawn")
	require.True(t, m.terminalSpawnCircuitOpen)
	require.Contains(t, m.status, "auto-spawn suspended")

	// Further rapid polls stay suspended.
	for i := 0; i < 20; i++ {
		now = now.Add(time.Second)
		m, spawned = driveEmptyReconcile(t, m, f)
		require.False(t, spawned, "poll %d must not spawn while circuit is open", i)
	}
	require.True(t, m.terminalSpawnCircuitOpen)
}

// #465: successful Spawn callbacks followed by repeatedly empty/stale listings
// must stay rate-limited — the production runaway was often "spawn OK" then a
// list that still showed zero terminals, not only hard spawn errors.
func TestReconcileTerminalAutoSpawnBoundsSuccessfulButEmptyListings(t *testing.T) {
	base := time.Date(2026, 9, 26, 14, 0, 0, 0, time.UTC)
	now := base
	old := terminalSpawnNow
	terminalSpawnNow = func() time.Time { return now }
	t.Cleanup(func() { terminalSpawnNow = old })

	f := &fakeAPI{}
	m := newListPane(f, "%9", "%1")
	totalSpawns := 0

	// Attempt 1: spawn succeeds (returns an id) but the list stays empty.
	var spawned bool
	m, spawned = driveEmptyReconcileSuccess(t, m, f, "t-phantom-1")
	require.True(t, spawned)
	totalSpawns++
	require.Equal(t, 1, m.terminalSpawnAttempts)
	require.Equal(t, "t-phantom-1", m.openedTerminal)
	require.False(t, m.terminalSpawnCircuitOpen)

	// Immediate empty re-poll while still inside backoff: no extra spawn, budget intact.
	m, spawned = driveEmptyReconcileSuccess(t, m, f, "t-phantom-should-not")
	require.False(t, spawned, "success+empty must not re-spawn inside backoff")
	require.Equal(t, 1, m.terminalSpawnAttempts, "success callback must not reset attempts")

	// Drain the remaining attempt budget with success+empty cycles past backoff.
	backoffs := []time.Duration{
		terminalSpawnInitialBackoff,     // → attempt 2
		2 * terminalSpawnInitialBackoff, // → attempt 3
	}
	elapsed := time.Duration(0)
	for i, wait := range backoffs {
		elapsed += wait
		now = base.Add(elapsed)
		id := fmt.Sprintf("t-phantom-%d", i+2)
		m, spawned = driveEmptyReconcileSuccess(t, m, f, id)
		require.True(t, spawned, "attempt %d after backoff", i+2)
		totalSpawns++
		require.Equal(t, i+2, m.terminalSpawnAttempts)
	}
	require.Equal(t, terminalSpawnMaxAttempts, m.terminalSpawnAttempts)
	require.Equal(t, terminalSpawnMaxAttempts, totalSpawns)

	// Past max attempts: circuit opens; further success-path empty polls stay quiet.
	now = base.Add(time.Hour)
	m, spawned = driveEmptyReconcileSuccess(t, m, f, "t-phantom-overflow")
	require.False(t, spawned)
	require.True(t, m.terminalSpawnCircuitOpen)
	require.Contains(t, m.status, "auto-spawn suspended")

	for i := 0; i < 50; i++ {
		now = now.Add(time.Second)
		before := m.terminalSpawnAttempts
		m, spawned = driveEmptyReconcileSuccess(t, m, f, fmt.Sprintf("t-overflow-%d", i))
		require.False(t, spawned, "rapid empty poll %d must not spawn", i)
		require.Equal(t, before, m.terminalSpawnAttempts, "attempts stay frozen once tripped")
		require.Equal(t, terminalSpawnMaxAttempts, m.terminalSpawnAttempts)
	}
	require.Equal(t, terminalSpawnMaxAttempts, totalSpawns, "total auto-spawns remain bounded")
}

// #465: a successful spawn callback alone must not clear the circuit / attempts
// even when the operator already tripped the breaker (stale success race).
func TestTerminalSpawnedSuccessDoesNotResetAttemptBudget(t *testing.T) {
	m := newListPane(&fakeAPI{}, "%9", "%1")
	m.terminalSpawnAttempts = 2
	m.terminalSpawnBackoff = 4 * time.Second
	m.terminalLastSpawnAt = time.Date(2026, 9, 26, 14, 0, 0, 0, time.UTC)
	m.terminalSpawnPending = true

	nm, cmd := m.Update(terminalSpawnedMsg{id: "t-ok", focus: false})
	m = nm.(controlPaneModel)
	require.Equal(t, 2, m.terminalSpawnAttempts, "success callback must not reset attempts")
	require.Equal(t, 4*time.Second, m.terminalSpawnBackoff)
	require.False(t, m.terminalSpawnPending)
	require.Equal(t, "t-ok", m.openedTerminal)
	require.False(t, m.terminalSpawnCircuitOpen)
	require.NotNil(t, cmd, "success still refreshes the list / opens the pane")
}

// #465: confirming a live terminal in m.sessions resets the attempt counter and
// closes the circuit so future deaths can auto-spawn again.
func TestReconcileTerminalAutoSpawnResetsWhenLiveConfirmed(t *testing.T) {
	f := &fakeAPI{}
	m := newListPane(f, "%9", "%1")
	m.defaultTerminalReady = true
	m.terminalSpawnAttempts = terminalSpawnMaxAttempts
	m.terminalSpawnCircuitOpen = true
	m.terminalSpawnBackoff = terminalSpawnMaxBackoff
	m.status = "terminal auto-spawn suspended — press t to create one manually"

	nm, cmd := m.Update(sessionsMsg{sessions: []*store.Session{liveTerminal("t1", "/w", time.Now())}})
	m = nm.(controlPaneModel)
	require.Equal(t, 0, m.terminalSpawnAttempts)
	require.False(t, m.terminalSpawnCircuitOpen)
	require.Zero(t, m.terminalSpawnBackoff)
	require.Nil(t, f.spawned)
	_ = cmd // re-attach / adopt may return a cmd; not a spawn
}

// #465: a manual `t`→create clears the circuit breaker so the user can recover.
func TestManualTerminalCreateClearsCircuitBreaker(t *testing.T) {
	f := &fakeAPI{}
	m := newListPane(f, "%9", "%1")
	m.defaultTerminalReady = true
	m.terminalSpawnCircuitOpen = true
	m.terminalSpawnAttempts = terminalSpawnMaxAttempts
	m.openedAgentDir = "/opened/dir"

	m = lstep(m, key("t"))
	require.Equal(t, modeTerminalChoice, m.mode)
	nm, cmd := m.Update(key("c"))
	m = nm.(controlPaneModel)
	require.False(t, m.terminalSpawnCircuitOpen)
	require.Equal(t, 0, m.terminalSpawnAttempts)
	require.True(t, m.terminalSpawnPending)
	require.NotNil(t, cmd)
	cmd()
	require.NotNil(t, f.spawned)
}
