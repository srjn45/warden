package agentname

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/store"
)

func TestResolveSpawnNameExplicitUntouched(t *testing.T) {
	got := ResolveSpawnName(context.Background(), SpawnNameInput{
		Explicit: "my-name",
		Prompt:   "ignored",
	}, &stubRunner{out: "should-not-run"}, map[string]bool{"my-name": true})
	require.Equal(t, "my-name", got, "explicit names are not disambiguated")
}

func TestResolveSpawnNameAutopilotConvention(t *testing.T) {
	got := ResolveSpawnName(context.Background(), SpawnNameInput{
		Role:     "autopilot",
		PlanSlug: "ship-it",
	}, nil, nil)
	require.Equal(t, "AP:ship-it", got)
	require.NoError(t, store.ValidateName(got))
}

func TestResolveSpawnNameWorkerConvention(t *testing.T) {
	got := ResolveSpawnName(context.Background(), SpawnNameInput{
		Role:   "worker",
		TaskID: "Task3_Spawn",
	}, nil, nil)
	require.Equal(t, "wkr:Task3_Spawn", got)
	require.NoError(t, store.ValidateName(got))
}

func TestResolveSpawnNameBrainConvention(t *testing.T) {
	got := ResolveSpawnName(context.Background(), SpawnNameInput{
		Role:     "brain",
		TargetID: "mgr-1",
	}, nil, nil)
	require.Equal(t, "brain:mgr-1", got)
}

func TestResolveSpawnNamePipelineStage(t *testing.T) {
	got := ResolveSpawnName(context.Background(), SpawnNameInput{
		Pipe:  "demo-pipe",
		Stage: "analyze",
	}, nil, map[string]bool{"demo-pipe:analyze": true})
	require.Equal(t, "demo-pipe:analyze-2", got)
	require.NoError(t, store.ValidateName(got))
}

func TestResolveSpawnNamePromptUsesRunner(t *testing.T) {
	r := &stubRunner{out: "ws-leak-fix"}
	got := ResolveSpawnName(context.Background(), SpawnNameInput{
		Prompt: "Fix memory leak in websocket listener",
	}, r, nil)
	require.Equal(t, "ws-leak-fix", got)
}

func TestResolveSpawnNamePromptLessCodename(t *testing.T) {
	got := ResolveSpawnName(context.Background(), SpawnNameInput{}, nil, nil)
	require.NoError(t, store.ValidateName(got))
	require.Contains(t, got, "-")
}

func TestResolveSpawnNameWorkerWithoutTaskFallsThrough(t *testing.T) {
	r := &stubRunner{out: "auth-refactor"}
	got := ResolveSpawnName(context.Background(), SpawnNameInput{
		Role:   "worker",
		Prompt: "refactor auth",
	}, r, nil)
	require.Equal(t, "auth-refactor", got, "worker without task id uses prompt resolver")
}
