package planstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestReconcileObservedEvidence_RepairsMergedPRAndCleanedBranch(t *testing.T) {
	svc, store, _, fake := newTestService(t)
	ctx := context.Background()

	plan := &Plan{
		ID: "plan-recon01", ProjectID: "proj-1", Name: "recon",
		FilePath: "plans/in_progress/recon.yaml", Status: PlanStatusInProgress,
		Branches: []string{"feat/recon"},
		ActiveExecution: &PlanExecution{
			ID: "pe-recon01", PlanID: "plan-recon01",
			ExecutionMode: PlanModeManual, ExecutorID: "agent-1",
			PlanBranches: []string{"feat/recon"},
		},
	}
	require.NoError(t, store.Create(ctx, plan))
	require.NoError(t, store.AppendEvent(ctx, &PlanExecutionEvent{
		DedupKey: "plan-recon01:pe-recon01:branch_pushed:feat/recon",
		PlanID:   "plan-recon01", ExecutionID: "pe-recon01",
		Kind: EventKindBranchPushed, OccurredAt: time.Now().UTC(),
		Payload: &EventPayload{Branch: "feat/recon"},
	}))

	fake.responses["gh pr list --head feat/recon --state all --json number,url,state,headRefName,mergeCommit"] = fakeResp{
		out: `[{"number":42,"url":"https://example.com/pr/42","state":"MERGED","headRefName":"feat/recon","mergeCommit":{"oid":"deadbeef"}}]`,
	}
	fake.responses["git worktree list --porcelain"] = fakeResp{out: "worktree /tmp/root\nHEAD abc\nbranch refs/heads/main\n"}
	fake.responses["git show-ref --verify --quiet refs/heads/feat/recon"] = fakeResp{err: errors.New("exit 1")}

	rep, err := svc.ReconcileObservedEvidence(ctx, plan.ID)
	require.NoError(t, err)
	require.Equal(t, 3, rep.Repaired, "pr_merged + branch_landed + worktree_removed")
	require.Contains(t, rep.Branches, "feat/recon")

	events, err := store.ListEvents(ctx, plan.ID, "pe-recon01")
	require.NoError(t, err)
	kinds := map[EventKind]int{}
	for _, ev := range events {
		kinds[ev.Kind]++
	}
	require.Equal(t, 1, kinds[EventKindPRMerged])
	require.Equal(t, 1, kinds[EventKindBranchLanded])
	require.Equal(t, 1, kinds[EventKindWorktreeRemoved])

	// Idempotent: second pass repairs nothing.
	rep2, err := svc.ReconcileObservedEvidence(ctx, plan.ID)
	require.NoError(t, err)
	require.Equal(t, 0, rep2.Repaired)
}

func TestReconcileObservedEvidence_NoActiveExecution(t *testing.T) {
	svc, store, _, _ := newTestService(t)
	ctx := context.Background()
	require.NoError(t, store.Create(ctx, &Plan{
		ID: "plan-idle", ProjectID: "proj-1", Name: "idle",
		FilePath: "plans/pending/idle.yaml", Status: PlanStatusPending,
	}))
	rep, err := svc.ReconcileObservedEvidence(ctx, "plan-idle")
	require.NoError(t, err)
	require.Equal(t, 0, rep.Repaired)
}

func TestReduceEvents_IsOnlySummaryDerivation(t *testing.T) {
	// ExecutionSummary is derived solely from typed events — agent prose notes
	// never appear in the summary, even when present alongside events.
	events := []*PlanExecutionEvent{
		{
			ID: "ev-1", PlanID: "plan-x", ExecutionID: "pe-x",
			Kind: EventKindExecutionStarted, Seq: 1, OccurredAt: time.Unix(1, 0).UTC(),
			Payload: &EventPayload{PlanName: "x", Goal: "g", TasksTotal: 1, ExecutionMode: string(PlanModeManual)},
		},
		{
			ID: "ev-2", PlanID: "plan-x", ExecutionID: "pe-x",
			Kind: EventKindCompletionVerified, Seq: 2, OccurredAt: time.Unix(2, 0).UTC(),
		},
	}
	sum := ReduceEvents(events)
	require.Equal(t, "x", sum.PlanName)
	require.Equal(t, "completed", sum.OutcomeNote)
	require.NotNil(t, sum.CompletedAt)
}
