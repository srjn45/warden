package lifecycle

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/agentname"
	"github.com/srjn45/warden/internal/store"
)

type stubNameRunner struct{ out string }

func (s stubNameRunner) Run(_ context.Context, _ string) (string, error) { return s.out, nil }

func TestAssignSpawnNamePrompt(t *testing.T) {
	lc := New(&FakeRunner{}, &FakeConfig{})
	lc.NameRunner = stubNameRunner{out: "auth-refactor"}
	req := SpawnRequest{Prompt: "refactor auth", Cwd: t.TempDir()}
	lc.assignSpawnName(context.Background(), &req)
	require.Equal(t, "auth-refactor", req.Name)
}

func TestAssignSpawnNameCodenameWhenPromptLess(t *testing.T) {
	lc := New(&FakeRunner{}, &FakeConfig{})
	req := SpawnRequest{Cwd: t.TempDir()}
	lc.assignSpawnName(context.Background(), &req)
	require.NoError(t, store.ValidateName(req.Name))
}

func TestAssignSpawnNameAutopilotFromTicket(t *testing.T) {
	lc := New(&FakeRunner{}, &FakeConfig{})
	req := SpawnRequest{Role: "autopilot", Ticket: "release-autopilot", Prompt: "digest"}
	lc.assignSpawnName(context.Background(), &req)
	require.Equal(t, "AP:release", req.Name)
}

func TestAssignSpawnNameWorkerTask(t *testing.T) {
	lc := New(&FakeRunner{}, &FakeConfig{})
	req := SpawnRequest{Role: "worker", AutopilotTaskID: "Task3_spawn", Prompt: "implement"}
	lc.assignSpawnName(context.Background(), &req)
	require.Equal(t, "wkr:Task3_spawn", req.Name)
}

func TestAssignSpawnNameExplicitPreserved(t *testing.T) {
	lc := New(&FakeRunner{}, &FakeConfig{})
	lc.NameRunner = stubNameRunner{out: "should-not-win"}
	req := SpawnRequest{Name: "my-name", Prompt: "x"}
	lc.assignSpawnName(context.Background(), &req)
	require.Equal(t, "my-name", req.Name)
}

func TestAssignJobNamePipelineStage(t *testing.T) {
	lc := New(&FakeRunner{}, &FakeConfig{})
	name := lc.assignJobName(context.Background(), JobSpawnRequest{
		PipelineID: "demo", JobID: "analyze", Prompt: "look around",
		ExistingNames: map[string]bool{"demo:analyze": true},
	})
	require.Equal(t, "demo:analyze-2", name)
	require.NoError(t, store.ValidateName(name))
}

func TestSpawnAssignsName(t *testing.T) {
	lc := New(&FakeRunner{}, &FakeConfig{})
	lc.PromptsDir = t.TempDir()
	lc.NameRunner = agentname.RunnerFunc(func(_ context.Context, _ string) (string, error) {
		return "spawn-named", nil
	})
	sess, err := lc.Spawn(context.Background(), SpawnRequest{
		Prompt: "do a thing", Cwd: t.TempDir(),
	})
	require.NoError(t, err)
	require.Equal(t, "spawn-named", sess.Name)
}
