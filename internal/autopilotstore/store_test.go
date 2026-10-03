package autopilotstore

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/srjn45/scriva"
	"github.com/srjn45/scriva/engine"
	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/planstore"
)

func TestDisplayName(t *testing.T) {
	require.Equal(t, "AP:my-plan", DisplayName("my-plan"))
	require.Equal(t, "AP:my-plan", DisplayName("AP:my-plan"))
	require.Equal(t, "AP:unnamed", DisplayName(""))
	require.Equal(t, "AP:unnamed", DisplayName("  "))
}

func TestIsLiveState(t *testing.T) {
	for _, s := range []string{"starting", "active", "healing", "degraded", "paused"} {
		require.True(t, IsLiveState(s), s)
	}
	for _, s := range []string{"registered", "stopped", "complete", "disabled", ""} {
		require.False(t, IsLiveState(s), s)
	}
}

func TestCreateRequiresPlanID(t *testing.T) {
	s, err := New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	ctx := context.Background()

	err = s.Create(ctx, &Autopilot{ID: "ap-noplan", ProjectID: "/repo", Name: "AP:x"})
	require.ErrorIs(t, err, ErrPlanRequired)

	require.NoError(t, s.Create(ctx, &Autopilot{
		ID: "ap-ok", ProjectID: "/repo", PlanID: "plan-aabbccdd", Name: DisplayName("x"),
	}))
	got, err := s.Get(ctx, "ap-ok")
	require.NoError(t, err)
	require.Equal(t, "plan-aabbccdd", got.PlanID)
	require.Equal(t, "AP:x", got.Name)
}

// TestParentAgentIDPersistence pins Autopilot.parent_agent_id: omitted when empty
// so pre-field records read cleanly, and round-trips through ScrivaDB when set.
func TestParentAgentIDPersistence(t *testing.T) {
	raw, err := json.Marshal(Autopilot{ID: "ap-root", ProjectID: "/r", PlanID: "plan-1", Name: "AP:x"})
	require.NoError(t, err)
	require.NotContains(t, string(raw), "parent_agent_id")

	s, err := New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	ctx := context.Background()

	require.NoError(t, s.Create(ctx, &Autopilot{
		ID: "ap-child", ProjectID: "/r", PlanID: "plan-2", Name: "AP:nested",
		ParentAgentID: "agent-owner",
	}))
	got, err := s.Get(ctx, "ap-child")
	require.NoError(t, err)
	require.Equal(t, "agent-owner", got.ParentAgentID)

	// Legacy migrate path leaves ParentAgentID empty (no parent on old runs).
	require.Equal(t, "", (&Autopilot{ID: "ap-legacy"}).ParentAgentID)
}

func TestCRUDAndListFilters(t *testing.T) {
	s, err := New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	ctx := context.Background()

	a1 := &Autopilot{
		ID: "ap-aaa", ProjectID: "/p1", PlanID: "plan-1", Name: "AP:one",
		ManagerAgentID: "mgr-1", Diagnostics: Diagnostics{State: "active", Gate: "local"},
	}
	a2 := &Autopilot{
		ID: "ap-bbb", ProjectID: "/p1", PlanID: "plan-2", Name: "AP:two",
		BrainAgentID: "brain-1", Diagnostics: Diagnostics{State: "paused"},
	}
	a3 := &Autopilot{
		ID: "ap-ccc", ProjectID: "/p2", PlanID: "plan-3", Name: "AP:three",
	}
	require.NoError(t, s.Create(ctx, a1))
	require.NoError(t, s.Create(ctx, a2))
	require.NoError(t, s.Create(ctx, a3))
	require.ErrorIs(t, s.Create(ctx, a1), ErrExists)

	all, err := s.List(ctx)
	require.NoError(t, err)
	require.Len(t, all, 3)

	byProj, err := s.ListByProject(ctx, "/p1")
	require.NoError(t, err)
	require.Len(t, byProj, 2)

	byPlan, err := s.ListByPlan(ctx, "plan-2")
	require.NoError(t, err)
	require.Len(t, byPlan, 1)
	require.Equal(t, "brain-1", byPlan[0].BrainAgentID)

	updated, err := s.Update(ctx, "ap-aaa", func(a *Autopilot) error {
		a.Diagnostics.State = "healing"
		a.Diagnostics.LastError = "nudge failed"
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, "healing", updated.Diagnostics.State)

	// Clearing PlanID on update must fail.
	_, err = s.Update(ctx, "ap-aaa", func(a *Autopilot) error {
		a.PlanID = ""
		return nil
	})
	require.ErrorIs(t, err, ErrPlanRequired)

	require.NoError(t, s.Delete(ctx, "ap-bbb"))
	require.NoError(t, s.Delete(ctx, "ap-bbb")) // idempotent
	_, err = s.Get(ctx, "ap-bbb")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestMigrateLiveRunToAutopilotAndPlanHistory(t *testing.T) {
	data := t.TempDir()
	ctx := context.Background()

	plans, err := planstore.New(filepath.Join(data, "plans"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, plans.Close()) })

	plan := &planstore.Plan{
		ID:        "plan-live0001",
		ProjectID: "/repo",
		Name:      "entity-redesign",
		FilePath:  "plans/in_progress/entity-redesign.yaml",
		Status:    planstore.PlanStatusInProgress,
	}
	require.NoError(t, plans.Create(ctx, plan))

	now := time.Now().UTC().Truncate(time.Millisecond)
	seedLegacyRun(t, data, LegacyRunRecord{
		RunID:             "ap-aabbccddeeff",
		Name:              "entity-redesign",
		Repo:              "/repo",
		PlanFile:          filepath.Join("/repo", plan.FilePath),
		PlanID:            plan.ID,
		ProjectID:         "/repo",
		State:             "active",
		IntegrationBranch: "autopilot/entity-redesign",
		Gate:              "local",
		BrainID:           "entity-redesign-autopilot",
		SlotScope:         "entity-redesign",
		CreatedAt:         now,
		UpdatedAt:         now,
	})

	live, err := New(data)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, live.Close()) })

	rep, err := MigrateLegacyRuns(ctx, data, live, plans)
	require.NoError(t, err)
	require.Equal(t, 1, rep.LiveCreated)
	require.Equal(t, 1, rep.HistoryAttached)
	require.Equal(t, 0, rep.ArchivedUnmatched)

	got, err := live.Get(ctx, "ap-aabbccddeeff")
	require.NoError(t, err)
	require.Equal(t, plan.ID, got.PlanID)
	require.Equal(t, "AP:entity-redesign", got.Name)
	require.Equal(t, "entity-redesign-autopilot", got.ManagerAgentID)
	require.Empty(t, got.ParentAgentID, "legacy runs have no parent agent")
	require.Equal(t, "active", got.Diagnostics.State)
	require.Equal(t, "autopilot/entity-redesign", got.Diagnostics.IntegrationBranch)

	updated, err := plans.Get(ctx, plan.ID)
	require.NoError(t, err)
	require.NotNil(t, updated.ActiveExecution)
	require.Equal(t, "ap-aabbccddeeff", updated.ActiveExecution.ExecutorID)
	require.Equal(t, planstore.PlanModeAutopilot, updated.ActiveExecution.ExecutionMode)

	events, err := plans.ListEvents(ctx, plan.ID, updated.ActiveExecution.ID)
	require.NoError(t, err)
	require.NotEmpty(t, events)
	kinds := map[planstore.EventKind]bool{}
	for _, ev := range events {
		kinds[ev.Kind] = true
	}
	require.True(t, kinds[planstore.EventKindExecutionStarted])
	require.True(t, kinds[planstore.EventKindExecutorCreated])
	require.True(t, kinds[planstore.EventKindAgentSpawned])

	// Idempotent second pass.
	rep2, err := MigrateLegacyRuns(ctx, data, live, plans)
	require.NoError(t, err)
	require.False(t, rep2.Changed())
	require.Equal(t, 1, rep2.Skipped)
	list, err := live.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)
}

func TestMigrateCompletedRunAttachesHistoryWithoutLiveAutopilot(t *testing.T) {
	data := t.TempDir()
	ctx := context.Background()

	plans, err := planstore.New(filepath.Join(data, "plans"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, plans.Close()) })

	plan := &planstore.Plan{
		ID:        "plan-done0001",
		ProjectID: "/repo",
		Name:      "done-plan",
		FilePath:  "plans/completed/done-plan.yaml",
		Status:    planstore.PlanStatusCompleted,
	}
	require.NoError(t, plans.Create(ctx, plan))

	now := time.Now().UTC().Truncate(time.Millisecond)
	seedLegacyRun(t, data, LegacyRunRecord{
		RunID:     "ap-completed01",
		Name:      "done-plan",
		Repo:      "/repo",
		PlanFile:  plan.FilePath,
		PlanID:    plan.ID,
		ProjectID: "/repo",
		State:     "complete",
		CreatedAt: now.Add(-time.Hour),
		UpdatedAt: now,
	})

	live, err := New(data)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, live.Close()) })

	rep, err := MigrateLegacyRuns(ctx, data, live, plans)
	require.NoError(t, err)
	require.Equal(t, 0, rep.LiveCreated)
	require.Equal(t, 1, rep.HistoryAttached)

	list, err := live.List(ctx)
	require.NoError(t, err)
	require.Empty(t, list, "completed executors must not become live Autopilots")

	updated, err := plans.Get(ctx, plan.ID)
	require.NoError(t, err)
	require.Nil(t, updated.ActiveExecution)
	require.Len(t, updated.ExecutionHistory, 1)
	require.Equal(t, "ap-completed01", updated.ExecutionHistory[0].ExecutorID)
	require.Equal(t, planstore.ExecutionStatusCompleted, updated.ExecutionHistory[0].TerminalStatus)

	events, err := plans.ListEvents(ctx, plan.ID, updated.ExecutionHistory[0].ID)
	require.NoError(t, err)
	var sawComplete bool
	for _, ev := range events {
		if ev.Kind == planstore.EventKindCompletionVerified {
			sawComplete = true
		}
	}
	require.True(t, sawComplete)
}

func TestMigrateUnresolvedRunArchivesWithoutDelete(t *testing.T) {
	data := t.TempDir()
	ctx := context.Background()

	plans, err := planstore.New(filepath.Join(data, "plans"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, plans.Close()) })

	now := time.Now().UTC().Truncate(time.Millisecond)
	seedLegacyRun(t, data, LegacyRunRecord{
		RunID:     "ap-orphan0001",
		Name:      "ghost",
		Repo:      "/missing",
		PlanFile:  "/missing/plans/ghost.yaml",
		State:     "stopped",
		CreatedAt: now,
		UpdatedAt: now,
	})

	live, err := New(data)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, live.Close()) })

	rep, err := MigrateLegacyRuns(ctx, data, live, plans)
	require.NoError(t, err)
	require.Equal(t, 1, rep.ArchivedUnmatched)
	require.Equal(t, 0, rep.LiveCreated)

	archived, err := ListLegacyArchive(data)
	require.NoError(t, err)
	require.Len(t, archived, 1)
	require.Equal(t, "ap-orphan0001", archived[0].RunID)

	// Source RunRecord must still exist (never silently deleted).
	src, err := listLegacyRuns(filepath.Join(data, filepath.FromSlash(legacyRunsRelDir)))
	require.NoError(t, err)
	require.Len(t, src, 1)

	// Second pass: archive remains, no duplicate writes.
	rep2, err := MigrateLegacyRuns(ctx, data, live, plans)
	require.NoError(t, err)
	require.Equal(t, 0, rep2.ArchivedUnmatched)
	require.Equal(t, 1, rep2.Skipped)
	archived, err = ListLegacyArchive(data)
	require.NoError(t, err)
	require.Len(t, archived, 1)
}

func TestMigrateEmptyDataDirIsIdempotent(t *testing.T) {
	data := t.TempDir()
	live, err := New(data)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, live.Close()) })

	rep, err := MigrateLegacyRuns(context.Background(), data, live, nil)
	require.NoError(t, err)
	require.False(t, rep.Changed())
	_, err = os.Stat(filepath.Join(data, importedMarkerName))
	require.NoError(t, err)

	rep2, err := MigrateLegacyRuns(context.Background(), data, live, nil)
	require.NoError(t, err)
	require.False(t, rep2.Changed())
}

func seedLegacyRun(t *testing.T, dataDir string, run LegacyRunRecord) {
	t.Helper()
	dir := filepath.Join(dataDir, filepath.FromSlash(legacyRunsRelDir))
	require.NoError(t, os.MkdirAll(dir, 0o700))
	db, err := scriva.Open(dir, scriva.WithSyncMode(engine.SyncModeNone))
	require.NoError(t, err)
	defer db.Close()
	col, err := db.Collection(legacyCollection)
	require.NoError(t, err)
	b, err := json.Marshal(run)
	require.NoError(t, err)
	var rec map[string]any
	require.NoError(t, json.Unmarshal(b, &rec))
	_, _, err = col.InsertWithKey(run.RunID, rec)
	require.NoError(t, err)
}
