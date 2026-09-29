package tui

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// ── clampDim ─────────────────────────────────────────────────────────────────

func TestClampDimZero(t *testing.T) {
	require.Equal(t, minPaneDim, clampDim(0), "zero → minPaneDim")
}

func TestClampDimNegative(t *testing.T) {
	require.Equal(t, minPaneDim, clampDim(-99), "negative → minPaneDim")
}

func TestClampDimPositive(t *testing.T) {
	require.Equal(t, 80, clampDim(80), "positive passthrough")
}

func TestClampDimMinPaneDim(t *testing.T) {
	require.Equal(t, minPaneDim, clampDim(minPaneDim), "exact minimum passthrough")
}

// ── bodyH ────────────────────────────────────────────────────────────────────

func TestBodyHClampsZeroHeight(t *testing.T) {
	m := newListPane(&fakeAPI{}, "%9", "")
	m.h = 0
	require.Equal(t, 3, m.bodyH(), "h=0 must clamp to minimum 3")
}

func TestBodyHClampsNarrowHeight(t *testing.T) {
	m := newListPane(&fakeAPI{}, "%9", "")
	m.h = 2
	require.Equal(t, 3, m.bodyH(), "h=2 (below border overhead) must clamp to 3")
}

func TestBodyHNormalHeight(t *testing.T) {
	m := newListPane(&fakeAPI{}, "%9", "")
	m.h = 30
	require.Equal(t, 28, m.bodyH(), "h=30 → bodyH=28")
}

// ── WindowSizeMsg dimension clamping ─────────────────────────────────────────

// windowSizeStep advances the model with a WindowSizeMsg and returns the result.
func windowSizeStep(m controlPaneModel, w, h int) controlPaneModel {
	nm, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	return nm.(controlPaneModel)
}

func TestWindowSizeMsgZeroWidth(t *testing.T) {
	m := newListPane(&fakeAPI{}, "%9", "")
	m = windowSizeStep(m, 0, 24)
	require.GreaterOrEqual(t, m.w, minPaneDim, "w must be clamped ≥ minPaneDim")
	require.GreaterOrEqual(t, m.vp.Width, 1, "viewport width must be ≥ 1")
}

func TestWindowSizeMsgZeroHeight(t *testing.T) {
	m := newListPane(&fakeAPI{}, "%9", "")
	m = windowSizeStep(m, 80, 0)
	require.GreaterOrEqual(t, m.h, minPaneDim, "h must be clamped ≥ minPaneDim")
	require.GreaterOrEqual(t, m.vp.Height, 1, "viewport height must be ≥ 1")
}

func TestWindowSizeMsgBothZero(t *testing.T) {
	m := newListPane(&fakeAPI{}, "%9", "")
	m = windowSizeStep(m, 0, 0)
	require.GreaterOrEqual(t, m.w, minPaneDim)
	require.GreaterOrEqual(t, m.h, minPaneDim)
	require.GreaterOrEqual(t, m.vp.Width, 1)
	require.GreaterOrEqual(t, m.vp.Height, 1)
	require.True(t, m.ready, "model must become ready even at zero dims")
}

func TestWindowSizeMsgNarrowWidth(t *testing.T) {
	// Width of 10: ta gets max(1, 10-2)=8, ti gets max(1, 10-20)=1
	m := newListPane(&fakeAPI{}, "%9", "")
	m = windowSizeStep(m, 10, 24)
	require.Equal(t, 10, m.w, "w=10 is above minPaneDim, stored as-is")
	// Verify textinput width was not set to a negative value (max(1, 10-20)=1).
	require.GreaterOrEqual(t, m.ti.Width, 1, "textinput width must be ≥ 1")
}

func TestWindowSizeMsgNegativeWidth(t *testing.T) {
	m := newListPane(&fakeAPI{}, "%9", "")
	m = windowSizeStep(m, -5, 24)
	require.Equal(t, minPaneDim, m.w, "negative width clamped to minPaneDim")
	require.GreaterOrEqual(t, m.ti.Width, 1)
}

func TestWindowSizeMsgViewportPositive(t *testing.T) {
	m := newListPane(&fakeAPI{}, "%9", "")
	m = windowSizeStep(m, 120, 40)
	require.Equal(t, 120, m.w)
	require.Equal(t, 40, m.h)
	require.Equal(t, max(1, 120-4), m.vp.Width)
	require.Equal(t, max(1, m.bodyH()-2), m.vp.Height)
}

// ── reconcileAgentPaneCmd: no-op cases ───────────────────────────────────────

func TestReconcileAgentPaneNoOpWithoutAgentPane(t *testing.T) {
	m := newListPane(&fakeAPI{}, "", "")
	m.openedAgent = "agent-1"
	cmd := m.reconcileAgentPaneCmd()
	require.Nil(t, cmd, "no agentPane → no-op")
}

func TestReconcileAgentPaneNoOpWithoutOpenedAgent(t *testing.T) {
	m := newListPane(&fakeAPI{}, "%9", "")
	m.openedAgent = ""
	cmd := m.reconcileAgentPaneCmd()
	require.Nil(t, cmd, "no openedAgent → no-op")
}

func TestReconcileAgentPaneNoOpWhenPaneAlive(t *testing.T) {
	old := isTmuxPaneDead
	isTmuxPaneDead = func(string) bool { return false }
	t.Cleanup(func() { isTmuxPaneDead = old })

	m := newListPane(&fakeAPI{}, "%9", "")
	m.openedAgent = "agent-1"
	cmd := m.reconcileAgentPaneCmd()
	require.Nil(t, cmd, "alive pane → no-op, no reattach")
}

// ── reconcileAgentPaneCmd: reattach cases ────────────────────────────────────

func TestReconcileAgentPaneReattachesDeadPane(t *testing.T) {
	old := isTmuxPaneDead
	isTmuxPaneDead = func(string) bool { return true }
	t.Cleanup(func() { isTmuxPaneDead = old })

	m := newListPane(&fakeAPI{}, "%9", "")
	m.openedAgent = "agent-1"
	m.sessions = []*store.Session{{ID: "agent-1", TmuxSession: "tmux-sess-1", Status: store.StatusWorking}}

	cmd := m.reconcileAgentPaneCmd()
	require.NotNil(t, cmd, "dead pane + live agent → reattach cmd")
	require.Equal(t, 1, m.agentPaneReattachAttempts, "attempt counter advances")
}

func TestReconcileAgentPaneDeferWhenAgentNotYetLive(t *testing.T) {
	old := isTmuxPaneDead
	isTmuxPaneDead = func(string) bool { return true }
	t.Cleanup(func() { isTmuxPaneDead = old })

	m := newListPane(&fakeAPI{}, "%9", "")
	m.openedAgent = "agent-1"
	// No sessions yet — agent not in live list.
	m.sessions = nil

	cmd := m.reconcileAgentPaneCmd()
	require.Nil(t, cmd, "dead pane + agent not in list → defer, no cmd yet")
	require.Equal(t, 1, m.agentPaneReattachAttempts, "attempt is still counted")
}

// ── reconcileAgentPaneCmd: backoff ───────────────────────────────────────────

// driveDeadPaneReconcile drives N reconcile ticks with a dead pane and a live agent.
// It sets agentPaneNow to a fixed clock so backoff is deterministic.
func TestReconcileAgentPaneAppliesBackoff(t *testing.T) {
	old := isTmuxPaneDead
	isTmuxPaneDead = func(string) bool { return true }
	t.Cleanup(func() { isTmuxPaneDead = old })

	now := time.Now()
	oldNow := agentPaneNow
	agentPaneNow = func() time.Time { return now }
	t.Cleanup(func() { agentPaneNow = oldNow })

	m := newListPane(&fakeAPI{}, "%9", "")
	m.openedAgent = "agent-1"
	m.sessions = []*store.Session{{ID: "agent-1", TmuxSession: "sess-1", Status: store.StatusWorking}}

	// First call: no backoff yet, should get a cmd.
	cmd1 := m.reconcileAgentPaneCmd()
	require.NotNil(t, cmd1, "first attempt: cmd returned")
	require.Equal(t, 1, m.agentPaneReattachAttempts)
	require.Equal(t, agentPaneReattachInitialBackoff, m.agentPaneReattachBackoff)

	// Second call at the same instant: within backoff window, must be suppressed.
	cmd2 := m.reconcileAgentPaneCmd()
	require.Nil(t, cmd2, "second attempt within backoff window: suppressed")
	require.Equal(t, 1, m.agentPaneReattachAttempts, "counter must not advance during backoff")
}

func TestReconcileAgentPaneBackoffAllowsAfterWait(t *testing.T) {
	old := isTmuxPaneDead
	isTmuxPaneDead = func(string) bool { return true }
	t.Cleanup(func() { isTmuxPaneDead = old })

	now := time.Now()
	oldNow := agentPaneNow
	agentPaneNow = func() time.Time { return now }
	t.Cleanup(func() { agentPaneNow = oldNow })

	m := newListPane(&fakeAPI{}, "%9", "")
	m.openedAgent = "agent-1"
	m.sessions = []*store.Session{{ID: "agent-1", TmuxSession: "sess-1", Status: store.StatusWorking}}

	// First attempt.
	cmd1 := m.reconcileAgentPaneCmd()
	require.NotNil(t, cmd1)

	// Advance clock past the backoff window.
	now = now.Add(agentPaneReattachInitialBackoff + time.Millisecond)
	agentPaneNow = func() time.Time { return now }

	// Second attempt: should now fire.
	cmd2 := m.reconcileAgentPaneCmd()
	require.NotNil(t, cmd2, "after backoff window: cmd returned again")
	require.Equal(t, 2, m.agentPaneReattachAttempts)
}

// ── reconcileAgentPaneCmd: circuit breaker ───────────────────────────────────

func TestReconcileAgentPaneCircuitBreaker(t *testing.T) {
	old := isTmuxPaneDead
	isTmuxPaneDead = func(string) bool { return true }
	t.Cleanup(func() { isTmuxPaneDead = old })

	// Fast-forward clock so each attempt passes its backoff window instantly.
	oldNow := agentPaneNow
	t.Cleanup(func() { agentPaneNow = oldNow })

	m := newListPane(&fakeAPI{}, "%9", "")
	m.openedAgent = "agent-1"
	m.sessions = []*store.Session{{ID: "agent-1", TmuxSession: "sess-1", Status: store.StatusWorking}}

	// Drive agentPaneReattachMaxAttempts attempts, each time advancing clock past backoff.
	base := time.Now()
	elapsed := time.Duration(0)
	for range agentPaneReattachMaxAttempts {
		now := base.Add(elapsed)
		agentPaneNow = func() time.Time { return now }
		m.reconcileAgentPaneCmd()
		elapsed += m.agentPaneReattachBackoff + time.Millisecond
	}

	require.Equal(t, agentPaneReattachMaxAttempts, m.agentPaneReattachAttempts)

	// Next call trips the circuit breaker.
	now := base.Add(elapsed)
	agentPaneNow = func() time.Time { return now }
	cmd := m.reconcileAgentPaneCmd()
	require.Nil(t, cmd, "circuit open → no cmd")
	require.True(t, m.agentPaneReattachCircuitOpen, "circuit must be open")
	require.Contains(t, m.status, "agent pane recovery suspended")
}

// ── reconcileAgentPaneCmd: reset on alive pane ───────────────────────────────

func TestReconcileAgentPaneResetBackoffOnAlivePanem(t *testing.T) {
	deadNow := true
	old := isTmuxPaneDead
	isTmuxPaneDead = func(string) bool { return deadNow }
	t.Cleanup(func() { isTmuxPaneDead = old })

	m := newListPane(&fakeAPI{}, "%9", "")
	m.openedAgent = "agent-1"
	m.sessions = []*store.Session{{ID: "agent-1", TmuxSession: "sess-1", Status: store.StatusWorking}}

	// Accumulate some attempt state.
	m.reconcileAgentPaneCmd()
	require.Equal(t, 1, m.agentPaneReattachAttempts)

	// Pane becomes alive.
	deadNow = false

	m.reconcileAgentPaneCmd()
	require.Equal(t, 0, m.agentPaneReattachAttempts, "attempts reset when pane is alive")
	require.Equal(t, time.Duration(0), m.agentPaneReattachBackoff, "backoff reset when pane is alive")
	require.False(t, m.agentPaneReattachCircuitOpen, "circuit cleared when pane is alive")
}
