package planstore

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func seedInProgressPlan(t *testing.T, svc *PlanService, store *Store, root, name string) (*Plan, string) {
	t.Helper()
	ctx := context.Background()
	p, err := svc.Create(ctx, "proj-1", sampleCreate(name))
	require.NoError(t, err)
	p, err = svc.Transition(ctx, p.ID, PlanStatusInProgress, TransitionOptions{ExecutionMode: PlanModeManual})
	require.NoError(t, err)
	require.Equal(t, "", p.FilePath)

	execID := "pe-" + planSlug(name)
	now := time.Now().UTC()
	require.NoError(t, store.Update(ctx, p.ID, func(pl *Plan) error {
		pl.ActiveExecution = &PlanExecution{
			ID: execID, PlanID: pl.ID, ExecutionMode: PlanModeManual,
			ExecutorID: "agent-root", StartedAt: now, TerminalStatus: ExecutionStatusRunning,
		}
		pl.TaskProgress = map[string]string{"t1": "done", "t2": "done"}
		return nil
	}))

	require.NoError(t, store.AppendEvent(ctx, &PlanExecutionEvent{
		DedupKey: p.ID + ":" + execID + ":execution_started",
		PlanID:   p.ID, ExecutionID: execID, Kind: EventKindExecutionStarted, OccurredAt: now,
		Payload: &EventPayload{PlanName: name, ExecutionMode: string(PlanModeManual), TasksTotal: 2, ExecutorID: "agent-root"},
	}))
	p, err = store.Get(ctx, p.ID)
	require.NoError(t, err)
	return p, execID
}

func TestFinalize_Success(t *testing.T) {
	svc, store, root, fake := newTestService(t)
	ctx := context.Background()
	fake.responses["gh pr list"] = fakeResp{out: "[]"}

	p, execID := seedInProgressPlan(t, svc, store, root, "Finalize Success")
	var cleanupSawSummary bool
	res, err := svc.Finalize(ctx, p.ID, func(_ context.Context, pl *Plan) CleanupEvidence {
		cleanupSawSummary = pl.ExecutionSummary != nil
		return CleanupEvidence{DeletedIDs: []string{pl.ActiveExecution.ExecutorID}}
	})
	require.NoError(t, err)
	require.True(t, cleanupSawSummary, "summary must be persisted before cleanup")
	require.Equal(t, PlanStatusCompleted, res.Plan.Status)
	require.NotNil(t, res.Plan.ExecutionSummary)
	require.Equal(t, "Finalize Success", res.Summary.PlanName)
	require.Nil(t, res.Plan.ActiveExecution)
	require.Len(t, res.Plan.ExecutionHistory, 1)
	require.Equal(t, execID, res.Plan.ExecutionHistory[0].ID)
	require.Equal(t, "", res.Plan.FilePath)
	_, err = os.Stat(filepath.Join(root, "plans"))
	require.True(t, os.IsNotExist(err), "finalize must not create plans/")

	events, err := store.ListEvents(ctx, p.ID, execID)
	require.NoError(t, err)
	require.NotEmpty(t, events, "events must survive executor cleanup / finalize")
}

func TestFinalize_UnmetRequirements(t *testing.T) {
	svc, store, _, fake := newTestService(t)
	ctx := context.Background()
	fake.responses["gh pr list"] = fakeResp{out: "[]"}

	p, err := svc.Create(ctx, "proj-1", sampleCreate("Unmet"))
	require.NoError(t, err)
	_, err = svc.Transition(ctx, p.ID, PlanStatusInProgress, TransitionOptions{ExecutionMode: PlanModeManual})
	require.NoError(t, err)

	cleanupCalled := false
	_, err = svc.Finalize(ctx, p.ID, func(context.Context, *Plan) CleanupEvidence {
		cleanupCalled = true
		return CleanupEvidence{}
	})
	require.Error(t, err)
	var ure *UnmetRequirementsError
	require.ErrorAs(t, err, &ure)
	require.False(t, cleanupCalled, "cleanup must not run when requirements unmet")

	got, err := store.Get(ctx, p.ID)
	require.NoError(t, err)
	require.Equal(t, PlanStatusInProgress, got.Status)
	require.Nil(t, got.ExecutionSummary)
}

func TestFinalize_GitHubReconciliation(t *testing.T) {
	svc, store, root, fake := newTestService(t)
	ctx := context.Background()

	p, execID := seedInProgressPlan(t, svc, store, root, "Reconcile Finalize")
	require.NoError(t, store.Update(ctx, p.ID, func(pl *Plan) error {
		pl.Branches = []string{"feat/recon"}
		pl.ActiveExecution.PlanBranches = []string{"feat/recon"}
		return nil
	}))
	now := time.Now().UTC()
	require.NoError(t, store.AppendEvent(ctx, &PlanExecutionEvent{
		DedupKey: p.ID + ":" + execID + ":branch_pushed:feat/recon",
		PlanID:   p.ID, ExecutionID: execID, Kind: EventKindBranchPushed, OccurredAt: now,
		Payload: &EventPayload{Branch: "feat/recon"},
	}))

	// Open PR list for evaluateCompletion (no open PRs).
	fake.responses["gh pr list --head feat/recon --state open"] = fakeResp{out: "[]"}
	// Reconciliation sees a MERGED PR and no local branch/worktree.
	fake.responses["gh pr list --head feat/recon --state all"] = fakeResp{out: `[{"number":9,"url":"https://example/pr/9","state":"MERGED","mergeCommit":{"oid":"abc"}}]`}
	fake.responses["git worktree list --porcelain"] = fakeResp{out: "worktree " + root + "\nbranch refs/heads/main\n"}
	fake.responses["git show-ref --verify --quiet refs/heads/feat/recon"] = fakeResp{err: os.ErrNotExist}

	res, err := svc.Finalize(ctx, p.ID, func(context.Context, *Plan) CleanupEvidence { return CleanupEvidence{} })
	require.NoError(t, err)
	require.Equal(t, PlanStatusCompleted, res.Plan.Status)
	require.Greater(t, res.Reconcile.Repaired, 0, "reconcile must repair merged PR / cleaned branch")

	events, err := store.ListEvents(ctx, p.ID, execID)
	require.NoError(t, err)
	kinds := map[EventKind]bool{}
	for _, ev := range events {
		kinds[ev.Kind] = true
	}
	require.True(t, kinds[EventKindPRMerged])
	require.True(t, kinds[EventKindWorktreeRemoved])
}

func TestFinalize_FailedCleanupKeepsInProgress(t *testing.T) {
	svc, store, root, fake := newTestService(t)
	ctx := context.Background()
	fake.responses["gh pr list"] = fakeResp{out: "[]"}

	p, _ := seedInProgressPlan(t, svc, store, root, "Cleanup Fail")
	_, err := svc.Finalize(ctx, p.ID, func(context.Context, *Plan) CleanupEvidence {
		return CleanupEvidence{
			Errors:     []string{"autopilot ap-x: boom"},
			PendingIDs: []string{"ap-x"},
		}
	})
	require.Error(t, err)
	var ce *CleanupIncompleteError
	require.ErrorAs(t, err, &ce)
	require.Equal(t, []string{"ap-x"}, ce.Evidence.PendingIDs)
	require.NotEmpty(t, ce.Summary.PlanID)

	got, err := store.Get(ctx, p.ID)
	require.NoError(t, err)
	require.Equal(t, PlanStatusInProgress, got.Status, "must stay in_progress on cleanup failure")
	require.NotNil(t, got.ExecutionSummary, "summary must be preserved")
	require.NotNil(t, got.CleanupEvidence)
	require.True(t, got.CleanupEvidence.Failed())
	require.Equal(t, "", got.FilePath)
}

func TestFinalize_RetryAfterCleanupFailure(t *testing.T) {
	svc, store, root, fake := newTestService(t)
	ctx := context.Background()
	fake.responses["gh pr list"] = fakeResp{out: "[]"}

	p, _ := seedInProgressPlan(t, svc, store, root, "Cleanup Retry")
	attempts := 0
	cleanup := func(context.Context, *Plan) CleanupEvidence {
		attempts++
		if attempts == 1 {
			return CleanupEvidence{Errors: []string{"still live"}, PendingIDs: []string{"agent-root"}}
		}
		return CleanupEvidence{DeletedIDs: []string{"agent-root"}}
	}

	_, err := svc.Finalize(ctx, p.ID, cleanup)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrCleanupIncomplete)

	first, err := store.Get(ctx, p.ID)
	require.NoError(t, err)
	summaryCopy := *first.ExecutionSummary

	res, err := svc.Finalize(ctx, p.ID, cleanup)
	require.NoError(t, err)
	require.Equal(t, 2, attempts)
	require.Equal(t, PlanStatusCompleted, res.Plan.Status)
	require.Equal(t, summaryCopy, *res.Plan.ExecutionSummary, "summary must not be overwritten on retry")
	require.Nil(t, res.Plan.CleanupEvidence)
}

func TestFinalize_ExecutorDeletionAfterSummaryPersistence(t *testing.T) {
	svc, store, root, fake := newTestService(t)
	ctx := context.Background()
	fake.responses["gh pr list"] = fakeResp{out: "[]"}

	p, _ := seedInProgressPlan(t, svc, store, root, "Order Check")
	var order []string
	res, err := svc.Finalize(ctx, p.ID, func(_ context.Context, pl *Plan) CleanupEvidence {
		require.NotNil(t, pl.ExecutionSummary, "summary before cleanup")
		order = append(order, "cleanup")
		require.Equal(t, PlanStatusInProgress, pl.Status)
		return CleanupEvidence{DeletedIDs: []string{"agent-root"}}
	})
	require.NoError(t, err)
	order = append(order, "completed")
	require.Equal(t, []string{"cleanup", "completed"}, order)
	require.Equal(t, PlanStatusCompleted, res.Plan.Status)
	require.Empty(t, res.Plan.OrchestratorID)
	require.Empty(t, res.Plan.AutopilotRunID)
	require.Empty(t, res.Plan.PipelineID)
}
