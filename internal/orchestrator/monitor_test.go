package orchestrator

import (
	"context"
	"errors"
	"testing"

	"github.com/srjn45/warden/internal/approval"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

type fakeCondenser struct{ line string }

func (f fakeCondenser) Condense(context.Context, string) (string, error) { return f.line, nil }

type failingCondenser struct{}

func (failingCondenser) Condense(context.Context, string) (string, error) {
	return "", errors.New("model down")
}

func TestMonitor_Stuck(t *testing.T) {
	fd := &fakeDaemon{sessions: []*store.Session{active("a1"), waitingApproval("a2"), errored("a3")}}
	m := NewMonitor(fd, fakeCondenser{line: "1 running; a2 awaiting approval; a3 errored"})
	out, err := m.FleetDigest(context.Background())
	require.NoError(t, err)
	require.Contains(t, out, "a2 awaiting approval")
}

func TestMonitor_CondenserFailureFallsBackToTable(t *testing.T) {
	m := NewMonitor(&fakeDaemon{sessions: []*store.Session{active("a1")}}, failingCondenser{})
	out, err := m.FleetDigest(context.Background())
	require.NoError(t, err) // degrades, never errors out
	require.Contains(t, out, "a1")
}

func TestMonitor_EmptyFleet(t *testing.T) {
	m := NewMonitor(&fakeDaemon{}, fakeCondenser{line: "x"})
	out, err := m.FleetDigest(context.Background())
	require.NoError(t, err)
	require.Contains(t, out, "nothing running")
}

func TestMonitor_NilCondenserUsesTable(t *testing.T) {
	m := NewMonitor(&fakeDaemon{sessions: []*store.Session{errored("a9")}}, nil)
	out, err := m.FleetDigest(context.Background())
	require.NoError(t, err)
	require.Contains(t, out, "a9")
}

func TestMonitor_AgentDigest(t *testing.T) {
	m := NewMonitor(&fakeDaemon{sessions: []*store.Session{active("a1")}}, fakeCondenser{line: "a1 is refactoring auth"})
	out, err := m.AgentDigest(context.Background(), "a1")
	require.NoError(t, err)
	require.Contains(t, out, "refactoring auth")
}

func TestMonitor_PendingForMe(t *testing.T) {
	fd := &fakeDaemon{apprOn: true, approvals: []approval.View{{ID: "a2", Question: "allow rm -rf?"}}}
	m := NewMonitor(fd, failingCondenser{}) // fall back to deterministic
	out, err := m.PendingForMe(context.Background())
	require.NoError(t, err)
	require.Contains(t, out, "a2")
	require.Contains(t, out, "allow rm -rf?")
}

func TestMonitor_PendingForMe_Empty(t *testing.T) {
	m := NewMonitor(&fakeDaemon{apprOn: true}, fakeCondenser{})
	out, err := m.PendingForMe(context.Background())
	require.NoError(t, err)
	require.Contains(t, out, "nothing waiting on you")
}

func TestMonitor_CleanupProposesOnlyTerminalAndGates(t *testing.T) {
	fd := &fakeDaemon{sessions: []*store.Session{active("a1"), done("a2"), errored("a3")}}
	spy := &spyGate{decision: Decision{Action: Reject}}
	m := NewMonitorWithGate(fd, fakeCondenser{}, spy)
	_, _ = m.CleanUp(context.Background())
	require.Equal(t, 1, spy.confirmCalls)
	require.ElementsMatch(t, []string{"a2", "a3"}, spy.proposedIDs())
	require.Zero(t, fd.terminateCalls, "reject reaps nothing")
}

func TestMonitor_CleanupApproveReaps(t *testing.T) {
	fd := &fakeDaemon{sessions: []*store.Session{active("a1"), done("a2"), errored("a3")}}
	spy := &spyGate{decision: Decision{Action: Approve}}
	m := NewMonitorWithGate(fd, fakeCondenser{}, spy)
	out, err := m.CleanUp(context.Background())
	require.NoError(t, err)
	require.Equal(t, 2, fd.terminateCalls)
	require.ElementsMatch(t, []string{"a2", "a3"}, fd.terminated)
	require.Contains(t, out, "a2")
}

func TestMonitor_CleanupNothingTerminal(t *testing.T) {
	fd := &fakeDaemon{sessions: []*store.Session{active("a1")}}
	spy := &spyGate{decision: Decision{Action: Approve}}
	m := NewMonitorWithGate(fd, fakeCondenser{}, spy)
	out, err := m.CleanUp(context.Background())
	require.NoError(t, err)
	require.Zero(t, spy.confirmCalls, "no gate prompt when nothing to reap")
	require.Contains(t, out, "nothing to clean up")
}
