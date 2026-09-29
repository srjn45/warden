package planstore

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestCompletionPolicy_remainsInProgressWhenEvidenceAbsent verifies that a plan
// stays in_progress and surfaces a structured UnmetRequirementsError when the
// task evidence is absent. Neither done_when text nor agent prose is consulted.
func TestCompletionPolicy_remainsInProgressWhenEvidenceAbsent(t *testing.T) {
	svc, store, _, fake := newTestService(t)
	ctx := context.Background()

	// Create and start a plan with two tasks.
	p, err := svc.Create(ctx, "proj-1", sampleCreate("Evidence Needed"))
	require.NoError(t, err)
	_, err = svc.Transition(ctx, p.ID, PlanStatusInProgress, TransitionOptions{ExecutionMode: PlanModeAutopilot})
	require.NoError(t, err)

	// Mark only t1 as done — t2 remains pending.
	_, err = svc.UpdateTaskStatus(ctx, p.ID, "t1", "done")
	require.NoError(t, err)

	// No gh PR response needed (plan has no branches at this point).
	fake.responses["gh pr list"] = fakeResp{out: "[]"}

	// Attempt to complete — must fail because t2 is still pending.
	_, err = svc.Transition(ctx, p.ID, PlanStatusCompleted, TransitionOptions{})
	require.Error(t, err, "plan with pending tasks must not complete")

	// The error must be UnmetRequirementsError with structured content.
	var ure *UnmetRequirementsError
	require.ErrorAs(t, err, &ure, "error must be *UnmetRequirementsError")

	reqs := ure.Requirements
	require.False(t, reqs.Satisfied, "requirements must not be satisfied")
	require.False(t, reqs.AllTasksDone, "AllTasksDone must be false")
	require.Equal(t, []string{"t2"}, reqs.PendingTaskIDs, "t2 must be listed as pending")
	require.True(t, reqs.NoOpenPRs, "no open PRs — only tasks are blocking")
	require.True(t, reqs.NoLiveAgents, "no events emitted → NoLiveAgents trivially true")
	require.True(t, reqs.ResourcesClean, "no branch events → ResourcesClean trivially true")

	// Legacy sentinel errors must still work for existing call sites.
	require.ErrorIs(t, err, ErrTasksIncomplete)
	var te *TasksIncompleteError
	require.ErrorAs(t, err, &te)
	require.Equal(t, []string{"t2"}, te.TaskIDs)

	// Plan must still be in_progress.
	p, err = store.Get(ctx, p.ID)
	require.NoError(t, err)
	require.Equal(t, PlanStatusInProgress, p.Status, "plan must remain in_progress")
}

// TestCompletionPolicy_completesWhenFullyEvidenced verifies that a plan
// transitions to completed when all five gates are satisfied via typed
// PlanExecutionEvents — without relying on agent textual reports, done_when
// strings, or UpdateTaskStatus calls.
func TestCompletionPolicy_completesWhenFullyEvidenced(t *testing.T) {
	svc, store, _, fake := newTestService(t)
	ctx := context.Background()

	// Create and start a plan.
	p, err := svc.Create(ctx, "proj-1", sampleCreate("Full Evidence"))
	require.NoError(t, err)
	_, err = svc.Transition(ctx, p.ID, PlanStatusInProgress, TransitionOptions{ExecutionMode: PlanModeAutopilot})
	require.NoError(t, err)

	// Attach an active execution so evaluateCompletion can load its events.
	execID := "pe-fulltest01"
	require.NoError(t, store.Update(ctx, p.ID, func(pl *Plan) error {
		pl.ActiveExecution = &PlanExecution{
			ID:            execID,
			PlanID:        pl.ID,
			ExecutionMode: PlanModeAutopilot,
			ExecutorID:    "ap-run-evidence01",
			StartedAt:     time.Now().UTC(),
		}
		return nil
	}))

	// Emit typed evidence events — deliberately NOT calling UpdateTaskStatus so
	// tasks stay "pending" in TaskProgress. Completion must derive entirely from
	// the event log, proving we do not trust agent text.
	now := time.Now().UTC()
	events := []*PlanExecutionEvent{
		{
			ID:          "ev-fe-start",
			PlanID:      p.ID,
			ExecutionID: execID,
			Kind:        EventKindExecutionStarted,
			OccurredAt:  now,
			Payload: &EventPayload{
				PlanName:      "Full Evidence",
				ExecutionMode: string(PlanModeAutopilot),
				TasksTotal:    2,
			},
		},
		{
			ID:          "ev-fe-t1",
			PlanID:      p.ID,
			ExecutionID: execID,
			Kind:        EventKindTaskEvidenceVerified,
			OccurredAt:  now.Add(time.Minute),
			Payload:     &EventPayload{TaskID: "t1"},
		},
		{
			ID:          "ev-fe-t2",
			PlanID:      p.ID,
			ExecutionID: execID,
			Kind:        EventKindTaskEvidenceVerified,
			OccurredAt:  now.Add(2 * time.Minute),
			Payload:     &EventPayload{TaskID: "t2"},
		},
		{
			ID:          "ev-fe-done",
			PlanID:      p.ID,
			ExecutionID: execID,
			Kind:        EventKindCompletionVerified,
			OccurredAt:  now.Add(3 * time.Minute),
		},
	}
	for _, ev := range events {
		require.NoError(t, store.AppendEvent(ctx, ev))
	}

	// Verify tasks are still "pending" in TaskProgress — we did NOT call UpdateTaskStatus.
	p, err = store.Get(ctx, p.ID)
	require.NoError(t, err)
	require.Equal(t, "pending", p.TaskProgress["t1"], "t1 must still be pending in TaskProgress")
	require.Equal(t, "pending", p.TaskProgress["t2"], "t2 must still be pending in TaskProgress")

	// No open PRs.
	fake.responses["gh pr list"] = fakeResp{out: "[]"}

	// Transition to completed — must succeed based purely on typed event evidence.
	got, err := svc.Transition(ctx, p.ID, PlanStatusCompleted, TransitionOptions{})
	require.NoError(t, err, "plan with full typed evidence must complete")
	require.Equal(t, PlanStatusCompleted, got.Status)
	require.NotNil(t, got.CompletedAt)
}

// TestCompletionPolicy_structuredErrorAllGates verifies that when multiple gates
// are unmet simultaneously, UnmetRequirementsError surfaces all of them at once
// rather than stopping at the first failure.
func TestCompletionPolicy_structuredErrorAllGates(t *testing.T) {
	svc, store, _, fake := newTestService(t)
	ctx := context.Background()

	p, err := svc.Create(ctx, "proj-1", sampleCreate("Multi Block"))
	require.NoError(t, err)
	_, err = svc.Transition(ctx, p.ID, PlanStatusInProgress, TransitionOptions{})
	require.NoError(t, err)

	// Add a branch with an open PR — and leave tasks incomplete.
	require.NoError(t, store.Update(ctx, p.ID, func(pl *Plan) error {
		pl.Branches = []string{"feat/multi-block"}
		return nil
	}))
	fake.responses["gh pr list --head feat/multi-block --state open --json number"] = fakeResp{
		out: `[{"number":99}]`,
	}

	// t1 is done, t2 is pending.
	_, err = svc.UpdateTaskStatus(ctx, p.ID, "t1", "done")
	require.NoError(t, err)

	_, err = svc.Transition(ctx, p.ID, PlanStatusCompleted, TransitionOptions{})
	require.Error(t, err)

	var ure *UnmetRequirementsError
	require.ErrorAs(t, err, &ure)

	// Both task and PR gates must be surfaced in one shot.
	require.False(t, ure.Requirements.AllTasksDone)
	require.Equal(t, []string{"t2"}, ure.Requirements.PendingTaskIDs)
	require.False(t, ure.Requirements.NoOpenPRs)
	require.Equal(t, []string{"feat/multi-block"}, ure.Requirements.OpenPRBranches)
	require.False(t, ure.Requirements.Satisfied)

	// Legacy errors still resolvable via the chain.
	require.ErrorIs(t, err, ErrTasksIncomplete)
	require.ErrorIs(t, err, ErrBranchesUnmerged)
}

// TestEvalCompletionFromEvents_liveAgentBlocks verifies that a started execution
// with no terminal event is correctly reported as having live agents, blocking
// completion.
func TestEvalCompletionFromEvents_liveAgentBlocks(t *testing.T) {
	plan := &Plan{
		TaskProgress: map[string]string{"t1": "done"},
		ActiveExecution: &PlanExecution{
			ExecutorID: "ap-run-live01",
		},
	}

	events := []*PlanExecutionEvent{
		{
			ID:          "ev-live-start",
			PlanID:      "plan-test",
			ExecutionID: "pe-live01",
			Kind:        EventKindExecutionStarted,
			OccurredAt:  time.Now().UTC(),
		},
		// No completion_verified / execution_failed / execution_stopped emitted.
	}

	reqs := EvalCompletionFromEvents(plan, []string{"t1"}, nil, events)

	require.True(t, reqs.AllTasksDone, "task is done via TaskProgress")
	require.True(t, reqs.NoOpenPRs, "no branches")
	require.False(t, reqs.NoLiveAgents, "execution started but not terminal")
	require.Equal(t, []string{"ap-run-live01"}, reqs.LiveExecutorIDs)
	require.False(t, reqs.Satisfied)
}

// TestEvalCompletionFromEvents_resourcesUnclean verifies that a branch that was
// pushed but whose worktree was not removed blocks completion.
func TestEvalCompletionFromEvents_resourcesUnclean(t *testing.T) {
	plan := &Plan{
		TaskProgress: map[string]string{"t1": "done"},
	}

	events := []*PlanExecutionEvent{
		{
			ID:          "ev-push-01",
			PlanID:      "plan-test",
			ExecutionID: "pe-res01",
			Kind:        EventKindBranchPushed,
			OccurredAt:  time.Now().UTC(),
			Payload:     &EventPayload{Branch: "feat/leftover"},
		},
		// No worktree_removed event for feat/leftover.
	}

	reqs := EvalCompletionFromEvents(plan, []string{"t1"}, nil, events)

	require.True(t, reqs.AllTasksDone)
	require.False(t, reqs.ResourcesClean, "pushed branch without worktree_removed is unclean")
	require.Equal(t, []string{"feat/leftover"}, reqs.UncleanBranches)
	require.False(t, reqs.Satisfied)
}

// TestEvalCompletionFromEvents_noEvents verifies that with no events, all
// event-derived gates are trivially satisfied and only TaskProgress governs.
func TestEvalCompletionFromEvents_noEvents(t *testing.T) {
	plan := &Plan{
		TaskProgress: map[string]string{"t1": "done", "t2": "skipped"},
	}

	reqs := EvalCompletionFromEvents(plan, []string{"t1", "t2"}, nil, nil)

	require.True(t, reqs.AllTasksDone)
	require.True(t, reqs.NoOpenPRs)
	require.True(t, reqs.ChecksPassing)
	require.True(t, reqs.NoLiveAgents)
	require.True(t, reqs.ResourcesClean)
	require.True(t, reqs.Satisfied)
}

// TestEvalCompletionFromEvents_doneWhenNotEvaluated ensures that free-text
// done_when guidance (carried in plan YAML, not in Plan struct) has no effect
// on event-based completion evaluation — the function is pure and never reads
// done_when.
func TestEvalCompletionFromEvents_doneWhenNotEvaluated(t *testing.T) {
	// done_when is a YAML-only field; it is NOT on the Plan struct.
	// The presence or absence of done_when text must have zero influence on
	// EvalCompletionFromEvents — verified here by the fact that a fully-evidenced
	// plan (all tasks done, no open PRs, no events) passes regardless.
	plan := &Plan{
		TaskProgress: map[string]string{"t1": "done"},
	}

	reqs := EvalCompletionFromEvents(plan, []string{"t1"}, nil, nil)
	require.True(t, reqs.Satisfied, "done_when must not block completion")
}
