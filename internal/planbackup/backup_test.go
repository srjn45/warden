package planbackup_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/planbackup"
	"github.com/srjn45/warden/internal/planstore"
)

func openStore(t *testing.T, dir string) *planstore.Store {
	t.Helper()
	st, err := planstore.New(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestBundleRoundTrip_freshDataDir(t *testing.T) {
	ctx := context.Background()
	srcDir := t.TempDir()
	src := openStore(t, srcDir)

	fixed := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	svc := planstore.NewPlanService(src, planstore.WithProjectRoot(func(string) string {
		return t.TempDir()
	}), planstore.WithNow(func() time.Time { return fixed }))

	p, err := svc.Create(ctx, "/proj/demo", planstore.CreateRequest{
		Name: "Backup Demo",
		Goal: "prove portable restore",
		Tasks: []planstore.TaskSpec{
			{ID: "design", Prompt: "write design"},
			{ID: "implement", Prompt: "ship it", After: []string{"design"}},
		},
		Constraints: []string{"no credentials in bundle"},
		DoneWhen:    []string{"round-trip works"},
	})
	require.NoError(t, err)
	require.Equal(t, planstore.PlanStatusPending, p.Status)

	execID := "pe-backup01"
	require.NoError(t, src.Update(ctx, p.ID, func(pl *planstore.Plan) error {
		pl.ExecutionHistory = []planstore.PlanExecution{{
			ID:             execID,
			PlanID:         pl.ID,
			ExecutionMode:  planstore.PlanModeManual,
			StartedAt:      fixed.Add(-time.Hour),
			CompletedAt:    ptrTime(fixed.Add(-time.Minute)),
			TerminalStatus: planstore.ExecutionStatusCompleted,
			TaskProgress:   map[string]string{"design": "done", "implement": "done"},
			Snapshot:       planstore.SnapshotFromPlan(pl),
		}}
		pl.ExecutionSummary = &planstore.ExecutionSummary{
			PlanID:        pl.ID,
			PlanName:      pl.Name,
			Goal:          pl.Goal,
			ExecutionMode: planstore.PlanModeManual,
			StartedAt:     fixed.Add(-time.Hour),
			CompletedAt:   ptrTime(fixed.Add(-time.Minute)),
			TasksTotal:    2,
			TasksDone:     2,
			OutcomeNote:   "completed",
		}
		pl.TaskProgress = map[string]string{"design": "done", "implement": "pending"}
		return nil
	}))

	require.NoError(t, src.AppendEvent(ctx, &planstore.PlanExecutionEvent{
		ID: "ev-backup-start", PlanID: p.ID, ExecutionID: execID,
		Kind: planstore.EventKindExecutionStarted, OccurredAt: fixed.Add(-time.Hour),
		Payload: &planstore.EventPayload{
			PlanName: p.Name, ExecutionMode: string(planstore.PlanModeManual), TasksTotal: 2,
		},
	}))
	require.NoError(t, src.AppendEvent(ctx, &planstore.PlanExecutionEvent{
		ID: "ev-backup-done", PlanID: p.ID, ExecutionID: execID,
		Kind: planstore.EventKindCompletionVerified, OccurredAt: fixed.Add(-time.Minute),
	}))
	require.NoError(t, src.AppendNote(ctx, &planstore.ExecutionNote{
		ID: "note-backup-1", PlanID: p.ID, ExecutionID: execID,
		AgentID: "agent-demo", Content: "shipped design", CreatedAt: fixed.Add(-30 * time.Minute),
	}))

	exporter := &planbackup.Exporter{Store: src, Now: func() time.Time { return fixed }}
	bundle, err := exporter.Export(ctx, planbackup.ExportOptions{PlanIDs: []string{p.ID}})
	require.NoError(t, err)
	require.Equal(t, planbackup.SchemaVersion, bundle.SchemaVersion)
	require.NotEmpty(t, bundle.BundleHash)
	require.Len(t, bundle.Entries, 1)
	require.Empty(t, bundle.Entries[0].Plan.AutopilotRunID)
	require.Nil(t, bundle.Entries[0].Plan.CleanupEvidence)
	require.Len(t, bundle.Entries[0].Events, 2)
	require.Len(t, bundle.Entries[0].Notes, 1)

	// Persist + reload as an operator would (JSON file transfer).
	raw, err := json.MarshalIndent(bundle, "", "  ")
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "plan-backup.json")
	require.NoError(t, os.WriteFile(path, raw, 0o600))
	loadedRaw, err := os.ReadFile(path)
	require.NoError(t, err)
	var loaded planbackup.Bundle
	require.NoError(t, json.Unmarshal(loadedRaw, &loaded))
	require.NoError(t, planbackup.Validate(&loaded))

	// Restore into a fresh ScrivaDB data directory — no Git, no plans/ replicas.
	dstDir := t.TempDir()
	dst := openStore(t, dstDir)
	restorer := &planbackup.Restorer{Store: dst}

	dry, err := restorer.Restore(ctx, &loaded, planbackup.RestoreOptions{DryRun: true})
	require.NoError(t, err)
	require.True(t, dry.DryRun)
	require.Equal(t, planbackup.OutcomeValidated, dry.Entries[0].Outcome)
	_, err = dst.Get(ctx, p.ID)
	require.ErrorIs(t, err, planstore.ErrNotFound, "dry-run must not write")

	res, err := restorer.Restore(ctx, &loaded, planbackup.RestoreOptions{})
	require.NoError(t, err)
	require.Equal(t, planbackup.OutcomeRestored, res.Entries[0].Outcome)

	// Idempotent retry.
	res2, err := restorer.Restore(ctx, &loaded, planbackup.RestoreOptions{})
	require.NoError(t, err)
	require.Equal(t, planbackup.OutcomeSkipped, res2.Entries[0].Outcome)

	// List / view from the fresh store.
	listed, err := dst.List(ctx)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	got, err := dst.Get(ctx, p.ID)
	require.NoError(t, err)
	require.Equal(t, p.Name, got.Name)
	require.Equal(t, p.Goal, got.Goal)
	require.Equal(t, p.ContentHash, got.ContentHash)
	require.Equal(t, p.Revision, got.Revision)
	require.Equal(t, planstore.PlanStatusPending, got.Status)
	require.NotNil(t, got.ExecutionSummary)
	require.Equal(t, "completed", got.ExecutionSummary.OutcomeNote)
	require.Len(t, got.ExecutionHistory, 1)

	events, err := dst.ListAllEvents(ctx, p.ID)
	require.NoError(t, err)
	require.Len(t, events, 2)
	notes, err := dst.ListAllNotes(ctx, p.ID)
	require.NoError(t, err)
	require.Len(t, notes, 1)

	// Execute normally on the restored Plan (no Git / no plans/ directory).
	dstSvc := planstore.NewPlanService(dst, planstore.WithProjectRoot(func(string) string {
		return t.TempDir()
	}))
	running, err := dstSvc.Transition(ctx, got.ID, planstore.PlanStatusInProgress, planstore.TransitionOptions{
		ExecutionMode: planstore.PlanModeManual,
	})
	require.NoError(t, err)
	require.Equal(t, planstore.PlanStatusInProgress, running.Status)
	require.Equal(t, planstore.PlanModeManual, running.ExecutionMode)

	updated, err := dstSvc.UpdateTaskStatus(ctx, got.ID, "implement", "done")
	require.NoError(t, err)
	require.Equal(t, "done", updated.TaskProgress["implement"])
}

func TestRestore_conflictPolicy(t *testing.T) {
	ctx := context.Background()
	src := openStore(t, t.TempDir())
	svc := planstore.NewPlanService(src, planstore.WithProjectRoot(func(string) string { return t.TempDir() }))
	p, err := svc.Create(ctx, "proj", planstore.CreateRequest{
		Name: "Conflict", Goal: "g",
		Tasks: []planstore.TaskSpec{{ID: "t1", Prompt: "p"}},
	})
	require.NoError(t, err)
	bundle, err := (&planbackup.Exporter{Store: src}).Export(ctx, planbackup.ExportOptions{PlanIDs: []string{p.ID}})
	require.NoError(t, err)

	dst := openStore(t, t.TempDir())
	// Seed a different definition under the same stable ID.
	other := *p
	other.Goal = "different goal"
	planstore.RefreshContentHash(&other)
	other.Revision = 99
	require.NoError(t, dst.RestorePlan(ctx, &other))

	_, err = (&planbackup.Restorer{Store: dst}).Restore(ctx, bundle, planbackup.RestoreOptions{OnConflict: planbackup.ConflictFail})
	require.Error(t, err)

	res, err := (&planbackup.Restorer{Store: dst}).Restore(ctx, bundle, planbackup.RestoreOptions{OnConflict: planbackup.ConflictSkip})
	require.NoError(t, err)
	require.Equal(t, planbackup.OutcomeConflicted, res.Entries[0].Outcome)
	still, err := dst.Get(ctx, p.ID)
	require.NoError(t, err)
	require.Equal(t, "different goal", still.Goal)

	res, err = (&planbackup.Restorer{Store: dst}).Restore(ctx, bundle, planbackup.RestoreOptions{OnConflict: planbackup.ConflictOverwrite})
	require.NoError(t, err)
	require.Equal(t, planbackup.OutcomeOverwritten, res.Entries[0].Outcome)
	got, err := dst.Get(ctx, p.ID)
	require.NoError(t, err)
	require.Equal(t, "g", got.Goal)
	require.Equal(t, p.Revision, got.Revision)
}

func TestValidate_tamperedHash(t *testing.T) {
	ctx := context.Background()
	src := openStore(t, t.TempDir())
	svc := planstore.NewPlanService(src, planstore.WithProjectRoot(func(string) string { return t.TempDir() }))
	p, err := svc.Create(ctx, "proj", planstore.CreateRequest{
		Name: "Tamper", Goal: "g",
		Tasks: []planstore.TaskSpec{{ID: "t1", Prompt: "p"}},
	})
	require.NoError(t, err)
	bundle, err := (&planbackup.Exporter{Store: src}).Export(ctx, planbackup.ExportOptions{PlanIDs: []string{p.ID}})
	require.NoError(t, err)
	bundle.BundleHash = "sha256:deadbeef"
	require.Error(t, planbackup.Validate(bundle))
}

func TestExport_excludesDisposableExecutorLinks(t *testing.T) {
	ctx := context.Background()
	src := openStore(t, t.TempDir())
	svc := planstore.NewPlanService(src, planstore.WithProjectRoot(func(string) string { return t.TempDir() }))
	p, err := svc.Create(ctx, "proj", planstore.CreateRequest{
		Name: "Live", Goal: "g",
		Tasks: []planstore.TaskSpec{{ID: "t1", Prompt: "p"}},
	})
	require.NoError(t, err)
	require.NoError(t, src.Update(ctx, p.ID, func(pl *planstore.Plan) error {
		pl.AutopilotRunID = "ap-secret"
		pl.PipelineID = "pipe-1"
		pl.ActiveExecution = &planstore.PlanExecution{
			ID: "pe-live", PlanID: pl.ID, ExecutorID: "agent-live",
			ExecutionMode: planstore.PlanModeAutopilot, StartedAt: time.Now().UTC(),
		}
		pl.CleanupEvidence = &planstore.CleanupEvidence{
			AttemptedAt: time.Now().UTC(),
			Errors:      []string{"worktree /tmp/x still present"},
		}
		pl.RemoteID = "hub-token-ish"
		return nil
	}))
	bundle, err := (&planbackup.Exporter{Store: src}).Export(ctx, planbackup.ExportOptions{PlanIDs: []string{p.ID}})
	require.NoError(t, err)
	e := bundle.Entries[0].Plan
	require.Empty(t, e.AutopilotRunID)
	require.Empty(t, e.PipelineID)
	require.Empty(t, e.RemoteID)
	require.Nil(t, e.CleanupEvidence)
	require.NotNil(t, e.ActiveExecution)
	require.Empty(t, e.ActiveExecution.ExecutorID)
	require.Equal(t, "pe-live", e.ActiveExecution.ID)
}

func ptrTime(t time.Time) *time.Time { return &t }
