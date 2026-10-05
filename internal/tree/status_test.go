package tree

import (
	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestAgentPresentationAndContainerRollup(t *testing.T) {
	code := 1
	require.Equal(t, "done", sessionStatus(store.StatusErrored, &code))
	for _, tt := range []struct {
		status               store.Status
		presented, container string
	}{
		{store.StatusSpawning, "pending", StatusActive},
		{store.StatusWorking, "busy", StatusActive},
		{store.StatusWaitingForInput, "need-input", StatusWaiting},
		{store.StatusIdle, "idle", StatusIdle},
		{store.StatusDone, "done", StatusDone},
		{store.StatusOrphaned, "orphaned", StatusError},
		{store.StatusErrored, "orphaned", StatusError},
		{store.StatusRateLimited, "rate_limited", StatusIdle},
	} {
		got := sessionStatus(tt.status)
		require.Equal(t, tt.presented, got)
		require.Equal(t, tt.container, rollup([]string{got}))
	}
	require.Equal(t, "busy", terminalStatus(store.StatusWorking))
	require.Equal(t, StatusActive, rollup([]string{"need-input", "busy"}))
	require.Equal(t, StatusError, rollup([]string{"busy", "orphaned"}))
}

func TestTerminalPresentation(t *testing.T) {
	code := 1
	for _, status := range []store.Status{store.StatusSpawning, store.StatusWorking, store.StatusIdle, store.StatusWaitingForInput, store.StatusRateLimited} {
		require.Equal(t, "busy", terminalStatus(status))
	}
	require.Equal(t, "done", terminalStatus(store.StatusDone))
	require.Equal(t, "done", terminalStatus(store.StatusErrored, &code))
	require.Equal(t, "orphaned", terminalStatus(store.StatusErrored))
	require.Equal(t, "orphaned", terminalStatus(store.StatusOrphaned))
}

func TestRunStatusNeedsAttentionIsWaiting(t *testing.T) {
	r := autopilot.RunStatus{State: autopilot.StateDegraded, NeedsAttention: "definition_error: x"}
	if got := runStatus(r); got != StatusWaiting {
		t.Fatalf("parked run status = %q, want %q", got, StatusWaiting)
	}
	r.NeedsAttention = ""
	if got := runStatus(r); got != StatusError {
		t.Fatalf("plain degraded status = %q, want %q", got, StatusError)
	}
}
