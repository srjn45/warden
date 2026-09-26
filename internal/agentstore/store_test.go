package agentstore

import (
	"context"
	"testing"

	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

func TestCRUD(t *testing.T) {
	s, err := New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	ctx := context.Background()
	a := &Agent{ID: "agent-1", Name: "worker", Status: store.StatusWorking, PipelineID: "p1", JobID: "build", Tags: []string{"autopilot"}, ParentID: "parent", ChildAgents: []string{"child"}}
	require.NoError(t, s.Insert(ctx, a))
	got, err := s.Get(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, a.PipelineID, got.PipelineID)
	require.Equal(t, a.ChildAgents, got.ChildAgents)
	require.NoError(t, s.Update(ctx, a.ID, func(a *Agent) error { a.Status = store.StatusIdle; return nil }))
	got, err = s.Get(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, store.StatusIdle, got.Status)
	list, err := s.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.NoError(t, s.Delete(ctx, a.ID))
	_, err = s.Get(ctx, a.ID)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestSpawnCreatesAndInitializesAgent(t *testing.T) {
	s, err := New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })

	a := &Agent{ID: "agent-spawn", Name: "worker", Status: store.StatusSpawning}
	require.NoError(t, s.Spawn(context.Background(), a, "tmux-agent-spawn", "ai-session-1"))
	got, err := s.Get(context.Background(), a.ID)
	require.NoError(t, err)
	require.Equal(t, "tmux-agent-spawn", got.TmuxSession)
	require.Equal(t, "ai-session-1", got.ClaudeSessionID)
}

func TestCreateCanonicalizesStatusAliases(t *testing.T) {
	s, err := New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	require.NoError(t, s.Create(context.Background(), &Agent{ID: "busy-agent", Status: store.Status("busy")}))
	got, err := s.Get(context.Background(), "busy-agent")
	require.NoError(t, err)
	require.Equal(t, store.StatusWorking, got.Status)
}

func TestTerminateAndDelete(t *testing.T) {
	s, err := New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	ctx := context.Background()
	require.NoError(t, s.Spawn(ctx, &Agent{ID: "agent-stop", Status: store.StatusWorking}, "tmux-stop", "ai-stop"))
	require.NoError(t, s.Terminate(ctx, "agent-stop"))
	got, err := s.Get(ctx, "agent-stop")
	require.NoError(t, err)
	require.Equal(t, store.StatusDone, got.Status)
	require.NoError(t, s.Delete(ctx, "agent-stop"))
	_, err = s.Get(ctx, "agent-stop")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestRecoverOnlyRevivesOrphanedAgent(t *testing.T) {
	s, err := New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	ctx := context.Background()
	require.NoError(t, s.Insert(ctx, &Agent{ID: "orphan", Status: store.StatusOrphaned}))
	require.NoError(t, s.Recover(ctx, "orphan"))
	got, err := s.Get(ctx, "orphan")
	require.NoError(t, err)
	require.Equal(t, store.StatusWorking, got.Status)

	require.NoError(t, s.Insert(ctx, &Agent{ID: "done", Status: store.StatusDone}))
	require.ErrorIs(t, s.Recover(ctx, "done"), ErrNotOrphaned)
}

func TestMigratesOnlyActiveNonTerminalSessions(t *testing.T) {
	dir := t.TempDir()
	legacy, err := store.NewFileStore(dir)
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, legacy.Insert(ctx, &store.Session{ID: "agent-live", Status: store.StatusWorking, AutopilotRunID: "ap", PipelineID: "p", JobID: "j", ChildPipelines: []string{"child"}}))
	require.NoError(t, legacy.Insert(ctx, &store.Session{ID: "terminal-live", Kind: store.KindTerminal, Status: store.StatusIdle}))
	require.NoError(t, legacy.Archive(ctx, "agent-live"))
	require.NoError(t, legacy.Insert(ctx, &store.Session{ID: "agent-current", Status: store.StatusWorking, Tags: []string{"tag"}}))
	require.NoError(t, legacy.Close(ctx))

	agents, err := New(dir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, agents.Close()) })
	list, err := agents.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, "agent-current", list[0].ID)
	require.Equal(t, []string{"tag"}, list[0].Tags)
}

func TestMigrationImportsExistingActiveAgentFields(t *testing.T) {
	dir := t.TempDir()
	legacy, err := store.NewFileStore(dir)
	require.NoError(t, err)
	ctx := context.Background()
	want := &store.Session{ID: "agent-live", Status: store.StatusWorking, PipelineID: "pipe", JobID: "job", Tags: []string{"autopilot"}, ParentID: "parent", ChildAgents: []string{"child"}, ChildPipelines: []string{"pipeline"}, AutopilotRunID: "run", AutopilotSlot: store.AutopilotSlotWorker, ProjectID: "project"}
	require.NoError(t, legacy.Insert(ctx, want))
	require.NoError(t, legacy.Close(ctx))
	agents, err := New(dir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, agents.Close()) })
	got, err := agents.Get(ctx, want.ID)
	require.NoError(t, err)
	require.Equal(t, want.PipelineID, got.PipelineID)
	require.Equal(t, want.JobID, got.JobID)
	require.Equal(t, want.Tags, got.Tags)
	require.Equal(t, want.ChildAgents, got.ChildAgents)
	require.Equal(t, want.ChildPipelines, got.ChildPipelines)
	require.Equal(t, want.AutopilotRunID, got.AutopilotRunID)
}

func TestMigrationMarkerPreventsDuplicateImport(t *testing.T) {
	dir := t.TempDir()
	legacy, err := store.NewFileStore(dir)
	require.NoError(t, err)
	require.NoError(t, legacy.Insert(context.Background(), &store.Session{ID: "agent-1"}))
	require.NoError(t, legacy.Close(context.Background()))
	s, err := New(dir)
	require.NoError(t, err)
	require.NoError(t, s.Close())
	// Reopen verifies the completed migration rather than trying to insert rows again.
	s, err = New(dir)
	require.NoError(t, err)
	require.NoError(t, s.Close())
}
