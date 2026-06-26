package orchestrator

import (
	"context"
	"testing"

	"github.com/srjn45/warden/internal/approval"
	"github.com/srjn45/warden/internal/client"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// TestDispatchEveryReadTool dispatches each read tool through the registry and
// asserts it routes to the daemon and surfaces the daemon's data. This pins the
// orchestrator's read surface to the Daemon interface contract (the tool the LLM
// names → the daemon call it makes).
func TestDispatchEveryReadTool(t *testing.T) {
	fd := &fakeDaemon{
		sessions:  []*store.Session{active("A-1")},
		apprOn:    true,
		approvals: []approval.View{{ID: "A-1", Recognized: true, Options: []string{"Yes"}}},
		inbox:     []client.Message{{ID: "m1", Body: "hi"}},
		ctxVal:    "the-value",
		ctxList:   []client.ContextEntry{{Key: "global.k", Value: "v"}},
		pipelines: []*pipeline.Pipeline{{ID: "p1"}},
	}
	reg := NewRegistry()
	ctx := context.Background()

	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"list_agents", nil, "A-1"},
		{"get_agent", map[string]any{"ticket": "A-1"}, "A-1"},
		{"get_agent_output", map[string]any{"ticket": "A-1"}, "output tail"},
		{"get_collaboration_status", nil, "null"},
		{"read_inbox", map[string]any{"agent": "A-1"}, "hi"},
		{"list_approvals", nil, "A-1"},
		{"ctx_get", map[string]any{"key": "global.k"}, "the-value"},
		{"ctx_list", map[string]any{}, "global.k"},
		{"pipeline_list", nil, "p1"},
		{"pipeline_get", map[string]any{"id": "p1"}, "p1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := reg.Dispatch(ctx, fd, ToolCall{Name: tc.name, Args: tc.args})
			require.NoError(t, err)
			require.Contains(t, out, tc.want)
		})
	}
}

// TestListApprovalsDisabledTool covers the approvals-off branch.
func TestListApprovalsDisabledTool(t *testing.T) {
	fd := &fakeDaemon{apprOn: false}
	out, err := NewRegistry().Dispatch(context.Background(), fd, ToolCall{Name: "list_approvals"})
	require.NoError(t, err)
	require.Contains(t, out, "disabled")
}

// TestGetAgentOutputDefaultLines verifies the default trailing-line count is
// applied when the model omits or under-specifies `lines`.
func TestGetAgentOutputDefaultLines(t *testing.T) {
	fd := &fakeDaemon{sessions: []*store.Session{active("A-1")}}
	out, err := NewRegistry().Dispatch(context.Background(), fd,
		ToolCall{Name: "get_agent_output", Args: map[string]any{"ticket": "A-1", "lines": float64(0)}})
	require.NoError(t, err)
	require.Equal(t, "output tail", out)
}

// TestDispatchEveryMutationTool dispatches each mutating tool and asserts the
// corresponding daemon mutation fired exactly once with the routed arguments.
func TestDispatchEveryMutationTool(t *testing.T) {
	reg := NewRegistry()
	ctx := context.Background()

	t.Run("spawn_agent", func(t *testing.T) {
		fd := &fakeDaemon{}
		out, err := reg.Dispatch(ctx, fd, ToolCall{Name: "spawn_agent",
			Args: map[string]any{"type": "development", "prompt": "do x", "worktree": true, "name": "scout"}})
		require.NoError(t, err)
		require.Contains(t, out, "spawned new-agent")
		require.Equal(t, 1, fd.spawnCalls)
		require.Equal(t, "development", fd.lastSpawn.Type)
		require.True(t, fd.lastSpawn.Worktree)
		require.Equal(t, "scout", fd.lastSpawn.Name)
	})

	t.Run("send_to_agent", func(t *testing.T) {
		fd := &fakeDaemon{}
		out, err := reg.Dispatch(ctx, fd, ToolCall{Name: "send_to_agent",
			Args: map[string]any{"ticket": "A-1", "text": "hello"}})
		require.NoError(t, err)
		require.Contains(t, out, "sent to A-1")
		require.Equal(t, 1, fd.inputCalls)
	})

	t.Run("terminate_agent", func(t *testing.T) {
		fd := &fakeDaemon{}
		out, err := reg.Dispatch(ctx, fd, ToolCall{Name: "terminate_agent", Args: map[string]any{"ticket": "A-1"}})
		require.NoError(t, err)
		require.Contains(t, out, "terminated A-1")
		require.Equal(t, []string{"A-1"}, fd.terminated)
	})

	t.Run("delete_agent", func(t *testing.T) {
		fd := &fakeDaemon{}
		_, err := reg.Dispatch(ctx, fd, ToolCall{Name: "delete_agent", Args: map[string]any{"ticket": "A-1", "hard": true}})
		require.NoError(t, err)
		require.Equal(t, []string{"A-1"}, fd.deleted)
	})

	t.Run("restore_agent", func(t *testing.T) {
		fd := &fakeDaemon{}
		_, err := reg.Dispatch(ctx, fd, ToolCall{Name: "restore_agent", Args: map[string]any{"ticket": "A-1"}})
		require.NoError(t, err)
		require.Equal(t, 1, fd.restoreCalls)
	})

	t.Run("approve", func(t *testing.T) {
		fd := &fakeDaemon{
			apprOn:    true,
			approvals: []approval.View{{ID: "A-1", Recognized: true, Options: []string{"Yes", "No"}, Fingerprint: "ff"}},
		}
		out, err := reg.Dispatch(ctx, fd, ToolCall{Name: "approve", Args: map[string]any{"ticket": "A-1", "option": float64(1)}})
		require.NoError(t, err)
		require.Contains(t, out, "approved A-1")
		require.Equal(t, 1, fd.approveCalls)
	})

	t.Run("approve out of range", func(t *testing.T) {
		fd := &fakeDaemon{
			apprOn:    true,
			approvals: []approval.View{{ID: "A-1", Options: []string{"Yes"}, Fingerprint: "ff"}},
		}
		_, err := reg.Dispatch(ctx, fd, ToolCall{Name: "approve", Args: map[string]any{"ticket": "A-1", "option": float64(9)}})
		require.Error(t, err)
		require.Equal(t, 0, fd.approveCalls, "an out-of-range option must not reach the daemon")
	})

	t.Run("commit", func(t *testing.T) {
		fd := &fakeDaemon{}
		_, err := reg.Dispatch(ctx, fd, ToolCall{Name: "commit", Args: map[string]any{"agent": "A-1", "message": "x"}})
		require.NoError(t, err)
		require.Equal(t, 1, fd.commitCalls)
	})

	t.Run("push", func(t *testing.T) {
		fd := &fakeDaemon{}
		_, err := reg.Dispatch(ctx, fd, ToolCall{Name: "push", Args: map[string]any{"agent": "A-1"}})
		require.NoError(t, err)
		require.Equal(t, 1, fd.pushCalls)
	})

	t.Run("sync", func(t *testing.T) {
		fd := &fakeDaemon{}
		_, err := reg.Dispatch(ctx, fd, ToolCall{Name: "sync", Args: map[string]any{"agent": "A-1", "base": "main"}})
		require.NoError(t, err)
		require.Equal(t, 1, fd.syncCalls)
	})

	t.Run("check", func(t *testing.T) {
		fd := &fakeDaemon{}
		_, err := reg.Dispatch(ctx, fd, ToolCall{Name: "check", Args: map[string]any{"agent": "A-1"}})
		require.NoError(t, err)
		require.Equal(t, 1, fd.checkCalls)
	})

	t.Run("ctx_set", func(t *testing.T) {
		fd := &fakeDaemon{}
		_, err := reg.Dispatch(ctx, fd, ToolCall{Name: "ctx_set", Args: map[string]any{"key": "k", "value": "v"}})
		require.NoError(t, err)
		require.Equal(t, 1, fd.ctxSetCalls)
	})

	t.Run("send_message", func(t *testing.T) {
		fd := &fakeDaemon{}
		_, err := reg.Dispatch(ctx, fd, ToolCall{Name: "send_message", Args: map[string]any{"to": "B-2", "body": "hi"}})
		require.NoError(t, err)
		require.Equal(t, 1, fd.msgSendCalls)
	})

	t.Run("pipeline_create", func(t *testing.T) {
		fd := &fakeDaemon{}
		out, err := reg.Dispatch(ctx, fd, ToolCall{Name: "pipeline_create", Args: map[string]any{"spec": "name: demo\njobs: []\n"}})
		require.NoError(t, err)
		require.Contains(t, out, "p1")
		require.Equal(t, 1, fd.pipeCreate)
	})

	t.Run("pipeline_cancel", func(t *testing.T) {
		fd := &fakeDaemon{}
		_, err := reg.Dispatch(ctx, fd, ToolCall{Name: "pipeline_cancel", Args: map[string]any{"id": "p1"}})
		require.NoError(t, err)
		require.Equal(t, 1, fd.pipeCancel)
	})
}

// TestMutationToolsRequireKeyArgs spot-checks that required-arg validation fires
// (and never panics) for the mutation tools, feeding the model a recoverable
// error rather than calling the daemon with empty ids.
func TestMutationToolsRequireKeyArgs(t *testing.T) {
	reg := NewRegistry()
	for _, name := range []string{"send_to_agent", "terminate_agent", "delete_agent", "restore_agent", "ctx_set"} {
		fd := &fakeDaemon{}
		_, err := reg.Dispatch(context.Background(), fd, ToolCall{Name: name, Args: map[string]any{}})
		require.Error(t, err, "%s must reject a missing required arg", name)
		require.Contains(t, err.Error(), "missing required argument")
	}
}
