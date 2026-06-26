package orchestrator

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRegistry_SplitMatchesSpec(t *testing.T) {
	reg := NewRegistry()
	readOnly := map[string]bool{"list_agents": true, "get_agent": true, "get_agent_output": true,
		"get_collaboration_status": true, "read_inbox": true, "list_approvals": true,
		"ctx_get": true, "ctx_list": true, "pipeline_list": true, "pipeline_get": true}
	for _, tl := range reg.Tools() {
		require.NotEmpty(t, tl.Schema.Name)
		require.NotEmpty(t, tl.Schema.Description)
		require.Equal(t, "object", tl.Schema.Parameters["type"], tl.Schema.Name)
		require.Equal(t, readOnly[tl.Schema.Name], !tl.Mutating, "side-effect flag for %s", tl.Schema.Name)
	}
}

func TestRegistry_HasNoCodeEditingTool(t *testing.T) {
	for _, tl := range NewRegistry().Tools() {
		require.NotContains(t, []string{"edit", "write", "bash", "shell", "exec", "run"}, tl.Schema.Name,
			"the orchestrator must have no code-editing/shell tool — it conducts, never implements")
	}
}

func TestDispatch_RoutesToDaemon(t *testing.T) {
	fd := &fakeDaemon{}
	reg := NewRegistry()
	_, err := reg.Dispatch(context.Background(), fd, ToolCall{Name: "spawn_agent",
		Args: map[string]any{"type": "development", "prompt": "refactor auth"}})
	require.NoError(t, err)
	require.Equal(t, "development", fd.lastSpawn.Type)
	require.Equal(t, "refactor auth", fd.lastSpawn.Prompt)
}

func TestDispatch_FreeFormSpawnDefaultsCwd(t *testing.T) {
	// A plain-prompt spawn (no type/repo) is the recommended path, but the daemon
	// requires a launch dir — the model never supplies one, so the registry must
	// default Cwd to the orchestrator's own working dir. Without this every
	// plain-prompt spawn is rejected.
	fd := &fakeDaemon{}
	_, err := NewRegistry().Dispatch(context.Background(), fd, ToolCall{Name: "spawn_agent",
		Args: map[string]any{"prompt": "review the docs"}})
	require.NoError(t, err)
	require.NotEmpty(t, fd.lastSpawn.Cwd, "free-form spawn must carry a launch dir")
}

func TestDispatch_TypedSpawnLeavesCwdEmpty(t *testing.T) {
	// A managed (type+repo) spawn does not launch in cwd, so we must not inject one.
	fd := &fakeDaemon{}
	_, err := NewRegistry().Dispatch(context.Background(), fd, ToolCall{Name: "spawn_agent",
		Args: map[string]any{"type": "development", "repo": "/some/repo", "prompt": "x"}})
	require.NoError(t, err)
	require.Empty(t, fd.lastSpawn.Cwd, "typed spawn must not get a cwd")
}

func TestDispatch_UnknownToolErrors(t *testing.T) {
	_, err := NewRegistry().Dispatch(context.Background(), &fakeDaemon{}, ToolCall{Name: "nope"})
	require.ErrorContains(t, err, "unknown tool")
}

func TestDispatch_MissingRequiredArgErrors(t *testing.T) {
	// A malformed map must error (fed back to the model), never panic.
	_, err := NewRegistry().Dispatch(context.Background(), &fakeDaemon{},
		ToolCall{Name: "get_agent", Args: map[string]any{}})
	require.ErrorContains(t, err, "missing required argument")
}

func TestToolSchemas_CoversEveryTool(t *testing.T) {
	reg := NewRegistry()
	require.Len(t, reg.ToolSchemas(), len(reg.Tools()))
}
