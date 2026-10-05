package lifecycle

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/fastbrain"
	"github.com/srjn45/warden/internal/store"
)

func nameLifecycle(fb *fakeFB) *Lifecycle {
	lc := New(&FakeRunner{}, &FakeConfig{})
	lc.FastBrain = fb
	return lc
}

func TestAssignSpawnNamePrompt(t *testing.T) {
	fb := &fakeFB{json: `{"name":"auth-refactor"}`}
	lc := nameLifecycle(fb)
	req := SpawnRequest{Prompt: "refactor auth", Cwd: t.TempDir()}
	lc.assignSpawnName(context.Background(), &req)
	require.Equal(t, "auth-refactor", req.Name)
	require.Len(t, fb.calls, 1)
	require.Equal(t, fastbrain.KindResolveAgentName, fb.calls[0].Kind)
	require.Equal(t, fastbrain.TierFast, fb.calls[0].Tier)
}

func TestAssignSpawnNameDisambiguatesEngineSlug(t *testing.T) {
	lc := nameLifecycle(&fakeFB{json: `{"name":"auth-refactor"}`})
	req := SpawnRequest{Prompt: "refactor auth", ExistingNames: map[string]bool{"auth-refactor": true}}
	lc.assignSpawnName(context.Background(), &req)
	require.Equal(t, "auth-refactor-2", req.Name)
}

func TestAssignSpawnNameCodenameOnEngineFailure(t *testing.T) {
	for name, fb := range map[string]*fakeFB{
		"timeout":      {status: fastbrain.StatusTimeout},
		"no_runner":    {status: fastbrain.StatusNoRunner},
		"invalid_json": {status: fastbrain.StatusInvalidJSON},
		"bad_slug":     {json: `{"name":"x"}`},
	} {
		lc := nameLifecycle(fb)
		req := SpawnRequest{Prompt: "refactor auth"}
		lc.assignSpawnName(context.Background(), &req)
		require.NoError(t, store.ValidateName(req.Name), name)
		require.Contains(t, req.Name, "-", name)
		require.Len(t, fb.calls, 1, name)
	}
}

func TestAssignSpawnNameNoEngineCodename(t *testing.T) {
	lc := New(&FakeRunner{}, &FakeConfig{})
	req := SpawnRequest{Prompt: "refactor auth"}
	lc.assignSpawnName(context.Background(), &req)
	require.NoError(t, store.ValidateName(req.Name))
}

func TestAssignSpawnNameCodenameWhenPromptLess(t *testing.T) {
	lc := New(&FakeRunner{}, &FakeConfig{})
	req := SpawnRequest{Cwd: t.TempDir()}
	lc.assignSpawnName(context.Background(), &req)
	require.NoError(t, store.ValidateName(req.Name))
}

func TestAssignSpawnNameAutopilotFromTicket(t *testing.T) {
	fb := &fakeFB{json: `{"name":"should-not-win"}`}
	lc := nameLifecycle(fb)
	defer func() { require.Empty(t, fb.calls) }()
	req := SpawnRequest{Role: "autopilot", Ticket: "release-autopilot", Prompt: "digest"}
	lc.assignSpawnName(context.Background(), &req)
	require.Equal(t, "AP:release", req.Name)
}

func TestAssignSpawnNameWorkerTask(t *testing.T) {
	fb := &fakeFB{json: `{"name":"should-not-win"}`}
	lc := nameLifecycle(fb)
	defer func() { require.Empty(t, fb.calls) }()
	req := SpawnRequest{Role: "worker", AutopilotTaskID: "Task3_spawn", Prompt: "implement"}
	lc.assignSpawnName(context.Background(), &req)
	require.Equal(t, "wkr:Task3_spawn", req.Name)
}

func TestAssignSpawnNameExplicitPreserved(t *testing.T) {
	fb := &fakeFB{json: `{"name":"should-not-win"}`}
	lc := nameLifecycle(fb)
	req := SpawnRequest{Name: "my-name", Prompt: "x"}
	lc.assignSpawnName(context.Background(), &req)
	require.Equal(t, "my-name", req.Name)
	require.Empty(t, fb.calls)
}

func TestAssignJobNamePipelineStage(t *testing.T) {
	fb := &fakeFB{json: `{"name":"should-not-win"}`}
	lc := nameLifecycle(fb)
	defer func() { require.Empty(t, fb.calls) }()
	name := lc.assignJobName(context.Background(), JobSpawnRequest{
		PipelineID: "demo", JobID: "analyze", Prompt: "look around",
		ExistingNames: map[string]bool{"demo:analyze": true},
	})
	require.Equal(t, "demo:analyze-2", name)
	require.NoError(t, store.ValidateName(name))
}

func TestSpawnAssignsName(t *testing.T) {
	lc := nameLifecycle(&fakeFB{json: `{"name":"spawn-named"}`})
	lc.PromptsDir = t.TempDir()
	sess, err := lc.Spawn(context.Background(), SpawnRequest{
		Prompt: "do a thing", Cwd: t.TempDir(),
	})
	require.NoError(t, err)
	require.Equal(t, "spawn-named", sess.Name)
}
