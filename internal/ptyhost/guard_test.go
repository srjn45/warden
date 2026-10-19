package ptyhost

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/ptyruntime"
)

// TestRoutedNewSessionTerminalOnlyGuard pins the existing behavior (§2.1 / §8.1 #9):
// routed.NewSession only branches to direct PTY for terminal:true and ModeOn.
// Agents (Host()) can NEVER reach the direct PTY branch regardless of mode (until 02b-03 flips it).
func TestRoutedNewSessionTerminalOnlyGuard(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "pty")
	run := newTmux(t)
	m := New(Config{
		Mode:           ModeOn,
		Dir:            dir,
		Run:            run,
		Shell:          []string{"/bin/sh"},
		ReconcileEvery: 50 * time.Millisecond,
		Options:        ptyruntime.Options{IdleFlush: 20 * time.Millisecond, KillGrace: 500 * time.Millisecond, DrainGrace: 300 * time.Millisecond},
	})
	defer m.Close()

	// 1. Agent session in ModeOn: must launch into tmux and NEVER be direct
	const agentName = "agent-guard-test"
	require.NoError(t, m.Host().NewSession(ctx, agentName, t.TempDir()))
	defer func() { _ = m.Host().KillSession(ctx, agentName) }()

	require.Nil(t, m.Direct(agentName), "agent in ModeOn must NOT be direct (routed.NewSession guard pins agent never direct)")
	require.False(t, m.IsAuthoritative(agentName), "mirrored agent is never authoritative")

	// 2. Terminal session in ModeOn: must launch into direct PTY
	const termName = "term-guard-test"
	require.NoError(t, m.TerminalHost().NewSession(ctx, termName, t.TempDir()))
	defer func() { _ = m.TerminalHost().KillSession(ctx, termName) }()

	require.NotNil(t, m.Direct(termName), "terminal in ModeOn must be direct on PTY")
	require.True(t, m.IsAuthoritative(termName), "direct terminal is authoritative")
}
