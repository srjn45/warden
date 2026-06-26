package orchestrator

import (
	"context"
	"testing"

	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// TestMonitoringToolsDispatch covers the supervision verbs AddMonitoring wires
// onto the registry: each is reachable by name through Dispatch and routes to the
// Monitor. This pins the model-callable supervision surface to the loop the same
// way the core tools are pinned.
func TestMonitoringToolsDispatch(t *testing.T) {
	fd := &fakeDaemon{
		sessions: []*store.Session{active("a1"), errored("a9")},
		apprOn:   true,
	}
	reg := NewRegistry()
	reg.AddMonitoring(NewMonitorWithGate(fd, fakeCondenser{line: "fleet ok"}, alwaysReject()))
	ctx := context.Background()

	// fleet_digest and pending_for_me take no args and summarize the fleet.
	out, err := reg.Dispatch(ctx, fd, ToolCall{Name: "fleet_digest"})
	require.NoError(t, err)
	require.NotEmpty(t, out)

	out, err = reg.Dispatch(ctx, fd, ToolCall{Name: "pending_for_me"})
	require.NoError(t, err)
	require.NotEmpty(t, out)

	// agent_digest requires a ticket and reports on that one agent.
	out, err = reg.Dispatch(ctx, fd, ToolCall{Name: "agent_digest", Args: map[string]any{"ticket": "a1"}})
	require.NoError(t, err)
	require.NotEmpty(t, out)

	// A missing ticket is a recoverable error, never a panic.
	_, err = reg.Dispatch(ctx, fd, ToolCall{Name: "agent_digest", Args: map[string]any{}})
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing required argument")

	// clean_up self-gates the teardown; with a rejecting gate nothing is reaped.
	out, err = reg.Dispatch(ctx, fd, ToolCall{Name: "clean_up"})
	require.NoError(t, err)
	require.NotEmpty(t, out)
	require.Equal(t, 0, fd.terminateCalls, "a rejected clean_up must not terminate anything")
}
