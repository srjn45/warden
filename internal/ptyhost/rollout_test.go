package ptyhost

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/ptyevent"
)

// F-ROLL-01: per-session runtime pin: flipping runtime.pty affects future
// launches only; live sessions keep their runtime and control (no unsafe live
// conversion, no loss of authority).
func TestF_ROLL_01_PerSessionRuntimePinAndCohortRollback(t *testing.T) {
	noLeaks(t)
	run := newTmux(t)
	dir := filepath.Join(t.TempDir(), "pty")
	m := newManager(t, run, dir, ModeOn)
	defer m.Close()

	ctx := context.Background()

	// 1. Launch a plain terminal under ModeOn: pinned to ModeOn (direct host).
	const termOn = "term-on"
	th := m.TerminalHost()
	require.NoError(t, th.NewSession(ctx, termOn, t.TempDir()))
	require.NotNil(t, m.Direct(termOn), "term-on must be hosted directly on PTY")
	require.True(t, m.IsAuthoritative(termOn), "term-on must be authoritative")
	pin, ok := m.PinnedRuntime(termOn)
	require.True(t, ok)
	require.Equal(t, ModeOn, pin)

	// 2. Configure cohort "canary" with ModeOn.
	const termCanary = "term-canary"
	m.SetCohortMode("canary", ModeOn)
	m.SetSessionCohort(termCanary, "canary")
	require.NoError(t, th.NewSession(ctx, termCanary, t.TempDir()))
	require.NotNil(t, m.Direct(termCanary), "term-canary must be hosted directly on PTY")
	require.True(t, m.IsAuthoritative(termCanary), "term-canary must be authoritative")

	// 3. Explicit pin override: pin term-pinned to ModeOff before creation.
	const termPinned = "term-pinned"
	m.SetSessionRuntimePin(termPinned, ModeOff)
	require.NoError(t, th.NewSession(ctx, termPinned, t.TempDir()))
	require.Nil(t, m.Direct(termPinned), "term-pinned explicitly pinned to off runs in tmux")
	require.False(t, m.IsAuthoritative(termPinned), "tmux session is not authoritative")

	// 4. Rollback: roll back cohort "canary" to ModeOff, and global mode to ModeOff.
	m.SetCohortMode("canary", ModeOff)
	m.SetMode(ModeOff)
	rollbacks := m.Rollbacks()
	require.NotEmpty(t, rollbacks, "rollback events must be logged in the ledger")

	// 5. Verify live sessions are untouched: they keep their runtime and control.
	require.NotNil(t, m.Direct(termOn), "term-on must stay hosted directly on PTY after rollback")
	require.True(t, m.IsAuthoritative(termOn), "term-on must stay authoritative after rollback")
	require.NotNil(t, m.Direct(termCanary), "term-canary must stay hosted directly on PTY after cohort rollback")
	require.True(t, m.IsAuthoritative(termCanary), "term-canary must stay authoritative after cohort rollback")

	// Control still works on the live direct sessions.
	require.NoError(t, th.SendKeys(ctx, termOn, "echo live-check", "Enter"))
	pane, err := th.CapturePane(ctx, termOn)
	require.NoError(t, err)
	_ = pane

	// 6. Future launches under rolled-back mode adopt the new mode (tmux).
	const termAfter = "term-after"
	require.NoError(t, th.NewSession(ctx, termAfter, t.TempDir()))
	require.Nil(t, m.Direct(termAfter), "session created after global rollback must run in tmux")
	require.False(t, m.IsAuthoritative(termAfter))

	const termCanaryAfter = "term-canary-after"
	m.SetSessionCohort(termCanaryAfter, "canary")
	require.NoError(t, th.NewSession(ctx, termCanaryAfter, t.TempDir()))
	require.Nil(t, m.Direct(termCanaryAfter), "session created after cohort rollback must run in tmux")
	require.False(t, m.IsAuthoritative(termCanaryAfter))

	// Clean up
	_ = th.KillSession(ctx, termOn)
	_ = th.KillSession(ctx, termCanary)
	_ = th.KillSession(ctx, termPinned)
	_ = th.KillSession(ctx, termAfter)
	_ = th.KillSession(ctx, termCanaryAfter)
}

// F-ROLL-03: shadow never mutates authoritative status and never sends input.
// An observe-only mirror cannot take over control or write to the pane.
func TestF_ROLL_03_ShadowNeverMutatesAuthoritativeStatusOrSendsInput(t *testing.T) {
	noLeaks(t)
	run := newTmux(t)
	dir := filepath.Join(t.TempDir(), "pty")
	m := newManager(t, run, dir, ModeShadow)
	defer m.Close()

	ctx := context.Background()
	const name = "agent-shadow-1"
	h := m.Host()
	require.NoError(t, h.NewSession(ctx, name, t.TempDir()))
	defer func() { _ = h.KillSession(ctx, name) }()

	// 1. Authoritative status check: shadow is never authoritative.
	require.False(t, m.IsAuthoritative(name), "shadow mode must never claim authoritative status")
	tg, ok := m.Lookup(name)
	require.True(t, ok)
	require.Nil(t, tg.Direct, "shadow target must not have a Direct PTY session")
	require.Equal(t, ptyevent.PresentationTmux, tg.Presentation)

	// 2. Transport connect: /pty WebSocket.
	url := wsServer(t, func() *Manager { return m }, name, false)
	c := dial(t, url)
	defer c.close()

	// Client requests interactive mode.
	c.send(ClientFrame{Type: "hello", Mode: ModeInteractive, ClientID: "viewer-1"})
	require.Eventually(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return len(c.plans) > 0
	}, waitFor, 10*time.Millisecond)

	c.mu.Lock()
	plan := c.plans[0]
	c.mu.Unlock()
	require.False(t, plan.Interactive, "shadow session must refuse interactive mode (forced observe-only)")
	require.False(t, plan.Controller, "shadow session must never grant controller lease")

	// 3. Send fenced input: must be rejected.
	c.send(ClientFrame{Type: "input", Gen: 1, ID: 1, Data: []byte("echo bad-input\n")})
	require.Eventually(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		for _, s := range c.srv {
			if s.Type == "rejected" {
				return true
			}
		}
		return false
	}, waitFor, 10*time.Millisecond)

	// 4. Send binary keystrokes: must be dropped.
	c.keys("echo secret-keystroke\n")
	time.Sleep(100 * time.Millisecond)

	pane, err := h.CapturePane(ctx, name)
	require.NoError(t, err)
	require.False(t, strings.Contains(pane, "secret-keystroke"), "shadow transport must never deliver keys to the pane")
	require.False(t, strings.Contains(pane, "bad-input"), "shadow transport must never deliver input to the pane")
}
