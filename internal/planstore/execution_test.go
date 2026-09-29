package planstore

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestNewPlanExecutionID verifies the pe-<8hex> format.
func TestNewPlanExecutionID(t *testing.T) {
	id := NewPlanExecutionID()
	require.Equal(t, 11, len(id), "expected pe-<8hex> (11 chars), got %q", id)
	require.Equal(t, "pe-", id[:3])
	for _, c := range id[3:] {
		require.True(t, (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f'),
			"expected lowercase hex digit in %q", id)
	}
	// Two calls must not collide.
	require.NotEqual(t, id, NewPlanExecutionID())
}

// TestPlanExecution_roundtrip verifies JSON encode/decode for PlanExecution.
func TestPlanExecution_roundtrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	pe := PlanExecution{
		ID:             "pe-aabbccdd",
		PlanID:         "plan-11223344",
		ExecutionMode:  PlanModeAutopilot,
		ExecutorID:     "ap-run-cafe0101",
		StartedAt:      now,
		TerminalStatus: ExecutionStatusRunning,
		TaskProgress:   map[string]string{"t1": "done", "t2": "in_progress"},
		PlanBranches:   []string{"autopilot/my-plan"},
	}

	b, err := json.Marshal(&pe)
	require.NoError(t, err)

	var got PlanExecution
	require.NoError(t, json.Unmarshal(b, &got))
	require.Equal(t, pe.ID, got.ID)
	require.Equal(t, pe.PlanID, got.PlanID)
	require.Equal(t, pe.ExecutionMode, got.ExecutionMode)
	require.Equal(t, pe.ExecutorID, got.ExecutorID)
	require.Equal(t, pe.StartedAt.UTC(), got.StartedAt.UTC())
	require.Equal(t, pe.TerminalStatus, got.TerminalStatus)
	require.Equal(t, pe.TaskProgress, got.TaskProgress)
	require.Equal(t, pe.PlanBranches, got.PlanBranches)
}

// TestExecutionSummary_roundtrip verifies JSON encode/decode for ExecutionSummary.
func TestExecutionSummary_roundtrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	done := now.Add(time.Hour)
	es := ExecutionSummary{
		PlanID:        "plan-aabb",
		PlanName:      "my-plan",
		Goal:          "ship it",
		ExecutionMode: PlanModePipeline,
		ExecutorID:    "pipe-001",
		StartedAt:     now,
		CompletedAt:   &done,
		TasksTotal:    5,
		TasksDone:     4,
		OutcomeNote:   "one task skipped",
	}

	b, err := json.Marshal(&es)
	require.NoError(t, err)

	var got ExecutionSummary
	require.NoError(t, json.Unmarshal(b, &got))
	require.Equal(t, es.PlanID, got.PlanID)
	require.Equal(t, es.PlanName, got.PlanName)
	require.Equal(t, es.Goal, got.Goal)
	require.Equal(t, es.ExecutionMode, got.ExecutionMode)
	require.Equal(t, es.TasksTotal, got.TasksTotal)
	require.Equal(t, es.TasksDone, got.TasksDone)
	require.Equal(t, es.OutcomeNote, got.OutcomeNote)
	require.NotNil(t, got.CompletedAt)
	require.Equal(t, done.UTC(), got.CompletedAt.UTC())
}

// TestPullRequestSummary_roundtrip verifies JSON encode/decode for PullRequestSummary.
func TestPullRequestSummary_roundtrip(t *testing.T) {
	mergedAt := time.Now().UTC().Truncate(time.Millisecond)
	pr := PullRequestSummary{
		URL:      "https://github.com/owner/repo/pull/42",
		Branch:   "feat/thing",
		Number:   42,
		State:    "merged",
		Title:    "Add thing",
		MergedAt: &mergedAt,
	}

	b, err := json.Marshal(&pr)
	require.NoError(t, err)

	var got PullRequestSummary
	require.NoError(t, json.Unmarshal(b, &got))
	require.Equal(t, pr.URL, got.URL)
	require.Equal(t, pr.Number, got.Number)
	require.Equal(t, pr.State, got.State)
	require.Equal(t, pr.Title, got.Title)
	require.NotNil(t, got.MergedAt)
	require.Equal(t, mergedAt.UTC(), got.MergedAt.UTC())
}

// TestTaskOutcome_roundtrip verifies JSON encode/decode for TaskOutcome.
func TestTaskOutcome_roundtrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	to := TaskOutcome{
		TaskID:        "task-alpha",
		Status:        "done",
		AssignedAgent: "agent-001",
		Branch:        "feat/alpha",
		PullRequests: []PullRequestSummary{
			{URL: "https://github.com/r/pulls/7", Branch: "feat/alpha", Number: 7, State: "merged"},
		},
		VerifiedChecks: []string{"build", "test", "lint"},
		CompletedAt:    now,
		Note:           "all checks passed",
	}

	b, err := json.Marshal(&to)
	require.NoError(t, err)

	var got TaskOutcome
	require.NoError(t, json.Unmarshal(b, &got))
	require.Equal(t, to.TaskID, got.TaskID)
	require.Equal(t, to.Status, got.Status)
	require.Equal(t, to.AssignedAgent, got.AssignedAgent)
	require.Equal(t, to.Branch, got.Branch)
	require.Len(t, got.PullRequests, 1)
	require.Equal(t, 7, got.PullRequests[0].Number)
	require.Equal(t, to.VerifiedChecks, got.VerifiedChecks)
	require.Equal(t, now.UTC(), got.CompletedAt.UTC())
	require.Equal(t, to.Note, got.Note)
}

// TestPlan_backwardCompatDecode verifies that an old Plan JSON record (without
// the new execution fields) decodes cleanly — all new fields default to zero.
func TestPlan_backwardCompatDecode(t *testing.T) {
	oldJSON := `{
		"id": "plan-aabbccdd",
		"project_id": "proj-1",
		"name": "feature-x",
		"file_path": "plans/pending/feature-x.yaml",
		"status": "pending",
		"task_progress": {"t1": "done"},
		"plan_branches": ["feat/feature-x"],
		"created_at": "2026-01-01T00:00:00Z",
		"updated_at": "2026-01-01T00:00:00Z"
	}`

	var p Plan
	require.NoError(t, json.Unmarshal([]byte(oldJSON), &p))

	require.Equal(t, "plan-aabbccdd", p.ID)
	require.Equal(t, PlanStatusPending, p.Status)
	require.Equal(t, map[string]string{"t1": "done"}, p.TaskProgress)

	// New fields must default to zero/nil.
	require.Nil(t, p.ActiveExecution, "ActiveExecution must default nil")
	require.Empty(t, p.ExecutionHistory, "ExecutionHistory must default empty")
	require.Nil(t, p.TaskOutcomes, "TaskOutcomes must default nil")
	require.Empty(t, p.BranchSummaries, "BranchSummaries must default empty")
}

// TestPlan_newFieldsRoundtrip verifies that a Plan with the new execution fields
// encodes and decodes faithfully through the store's encode/decode path.
func TestPlan_newFieldsRoundtrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)

	p := &Plan{
		ID:            "plan-aabbccdd",
		ProjectID:     "proj-1",
		Name:          "feature-x",
		FilePath:      "plans/in_progress/feature-x.yaml",
		Status:        PlanStatusInProgress,
		ExecutionMode: PlanModeAutopilot,
		TaskProgress:  map[string]string{"t1": "done", "t2": "in_progress"},
		ActiveExecution: &PlanExecution{
			ID:             "pe-11223344",
			PlanID:         "plan-aabbccdd",
			ExecutionMode:  PlanModeAutopilot,
			ExecutorID:     "ap-run-cafe01",
			StartedAt:      now,
			TerminalStatus: ExecutionStatusRunning,
			TaskProgress:   map[string]string{"t1": "done", "t2": "in_progress"},
			PlanBranches:   []string{"autopilot/feature-x"},
		},
		ExecutionHistory: []PlanExecution{
			{
				ID:             "pe-00aabbcc",
				PlanID:         "plan-aabbccdd",
				ExecutionMode:  PlanModePipeline,
				ExecutorID:     "pipe-old-01",
				StartedAt:      now.Add(-24 * time.Hour),
				TerminalStatus: ExecutionStatusFailed,
			},
		},
		TaskOutcomes: map[string]TaskOutcome{
			"t1": {
				TaskID:        "t1",
				Status:        "done",
				AssignedAgent: "agent-001",
				Branch:        "feat/t1",
				PullRequests: []PullRequestSummary{
					{URL: "https://github.com/r/pulls/42", Branch: "feat/t1", Number: 42, State: "merged"},
				},
				VerifiedChecks: []string{"build", "test"},
				CompletedAt:    now,
			},
		},
		BranchSummaries: []BranchSummary{
			{
				Name:    "feat/t1",
				TaskID:  "t1",
				AgentID: "agent-001",
				PR: &PullRequestSummary{
					URL:    "https://github.com/r/pulls/42",
					Branch: "feat/t1",
					Number: 42,
					State:  "merged",
				},
			},
		},
	}

	rec, err := encodeRecord(p)
	require.NoError(t, err)

	got, err := decodeRecord(rec)
	require.NoError(t, err)

	require.Equal(t, p.ID, got.ID)
	require.Equal(t, p.Status, got.Status)
	require.Equal(t, p.ExecutionMode, got.ExecutionMode)
	require.Equal(t, p.TaskProgress, got.TaskProgress)

	// ActiveExecution.
	require.NotNil(t, got.ActiveExecution)
	require.Equal(t, p.ActiveExecution.ID, got.ActiveExecution.ID)
	require.Equal(t, p.ActiveExecution.ExecutorID, got.ActiveExecution.ExecutorID)
	require.Equal(t, p.ActiveExecution.TerminalStatus, got.ActiveExecution.TerminalStatus)
	require.Equal(t, p.ActiveExecution.StartedAt.UTC(), got.ActiveExecution.StartedAt.UTC())
	require.Equal(t, p.ActiveExecution.PlanBranches, got.ActiveExecution.PlanBranches)
	require.Equal(t, p.ActiveExecution.TaskProgress, got.ActiveExecution.TaskProgress)

	// ExecutionHistory.
	require.Len(t, got.ExecutionHistory, 1)
	require.Equal(t, "pe-00aabbcc", got.ExecutionHistory[0].ID)
	require.Equal(t, ExecutionStatusFailed, got.ExecutionHistory[0].TerminalStatus)

	// TaskOutcomes.
	require.Len(t, got.TaskOutcomes, 1)
	outcome := got.TaskOutcomes["t1"]
	require.Equal(t, "done", outcome.Status)
	require.Equal(t, "agent-001", outcome.AssignedAgent)
	require.Len(t, outcome.PullRequests, 1)
	require.Equal(t, 42, outcome.PullRequests[0].Number)
	require.Equal(t, []string{"build", "test"}, outcome.VerifiedChecks)
	require.Equal(t, now.UTC(), outcome.CompletedAt.UTC())

	// BranchSummaries.
	require.Len(t, got.BranchSummaries, 1)
	bs := got.BranchSummaries[0]
	require.Equal(t, "feat/t1", bs.Name)
	require.Equal(t, "t1", bs.TaskID)
	require.Equal(t, "agent-001", bs.AgentID)
	require.NotNil(t, bs.PR)
	require.Equal(t, 42, bs.PR.Number)
}

// TestPlan_newFieldsStoreRoundtrip verifies the full Create→Get path through
// a real Store with the new execution fields populated.
func TestPlan_newFieldsStoreRoundtrip(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC().Truncate(time.Millisecond)

	p := &Plan{
		ID:            "plan-exec0001",
		ProjectID:     "proj-exec",
		Name:          "exec-test",
		FilePath:      "plans/in_progress/exec-test.yaml",
		Status:        PlanStatusInProgress,
		ExecutionMode: PlanModeOrchestratorWorker,
		TaskProgress:  map[string]string{"alpha": "in_progress"},
		ActiveExecution: &PlanExecution{
			ID:             NewPlanExecutionID(),
			PlanID:         "plan-exec0001",
			ExecutionMode:  PlanModeOrchestratorWorker,
			ExecutorID:     "agent-orch-01",
			StartedAt:      now,
			TerminalStatus: ExecutionStatusRunning,
			TaskProgress:   map[string]string{"alpha": "in_progress"},
		},
		TaskOutcomes:    map[string]TaskOutcome{},
		BranchSummaries: []BranchSummary{},
	}

	require.NoError(t, s.Create(context.Background(), p))

	got, err := s.Get(context.Background(), p.ID)
	require.NoError(t, err)

	require.Equal(t, PlanStatusInProgress, got.Status)
	require.NotNil(t, got.ActiveExecution)
	require.Equal(t, p.ActiveExecution.ExecutorID, got.ActiveExecution.ExecutorID)
	require.Equal(t, p.ActiveExecution.StartedAt.UTC(), got.ActiveExecution.StartedAt.UTC())
}

// TestCheckCompletionRequirements verifies the two Rule-5 gates.
func TestCheckCompletionRequirements(t *testing.T) {
	t.Run("all done no open prs", func(t *testing.T) {
		p := &Plan{TaskProgress: map[string]string{"t1": "done", "t2": "skipped"}}
		r := CheckCompletionRequirements(p, []string{"t1", "t2"}, nil)
		require.True(t, r.Satisfied)
		require.True(t, r.AllTasksDone)
		require.True(t, r.NoOpenPRs)
		require.Empty(t, r.PendingTaskIDs)
		require.Empty(t, r.OpenPRBranches)
	})

	t.Run("in_progress task blocks completion", func(t *testing.T) {
		p := &Plan{TaskProgress: map[string]string{"t1": "done", "t2": "in_progress"}}
		r := CheckCompletionRequirements(p, []string{"t1", "t2"}, nil)
		require.False(t, r.Satisfied)
		require.False(t, r.AllTasksDone)
		require.True(t, r.NoOpenPRs)
		require.Equal(t, []string{"t2"}, r.PendingTaskIDs)
	})

	t.Run("pending task blocks completion", func(t *testing.T) {
		p := &Plan{TaskProgress: map[string]string{"t1": "done"}}
		r := CheckCompletionRequirements(p, []string{"t1", "t2"}, nil)
		require.False(t, r.Satisfied)
		require.False(t, r.AllTasksDone)
		require.Equal(t, []string{"t2"}, r.PendingTaskIDs)
	})

	t.Run("open pr blocks completion", func(t *testing.T) {
		p := &Plan{TaskProgress: map[string]string{"t1": "done"}}
		r := CheckCompletionRequirements(p, []string{"t1"}, []string{"feat/t1"})
		require.False(t, r.Satisfied)
		require.True(t, r.AllTasksDone)
		require.False(t, r.NoOpenPRs)
		require.Equal(t, []string{"feat/t1"}, r.OpenPRBranches)
	})

	t.Run("nil task progress treats all tasks as pending", func(t *testing.T) {
		p := &Plan{}
		r := CheckCompletionRequirements(p, []string{"t1", "t2"}, nil)
		require.False(t, r.Satisfied)
		require.False(t, r.AllTasksDone)
		require.Len(t, r.PendingTaskIDs, 2)
	})

	t.Run("no tasks no branches is satisfied", func(t *testing.T) {
		p := &Plan{}
		r := CheckCompletionRequirements(p, nil, nil)
		require.True(t, r.Satisfied)
		require.True(t, r.AllTasksDone)
		require.True(t, r.NoOpenPRs)
	})

	t.Run("empty task ids are ignored", func(t *testing.T) {
		p := &Plan{TaskProgress: map[string]string{"t1": "done"}}
		r := CheckCompletionRequirements(p, []string{"", "t1", ""}, nil)
		require.True(t, r.Satisfied)
	})

	t.Run("done_when text is not evaluated", func(t *testing.T) {
		// Rule 5: done_when is free-text for humans, never a gate.
		// If tasks are done but done_when has content, result is still satisfied.
		p := &Plan{TaskProgress: map[string]string{"t1": "done"}}
		// done_when lives in YAML only; not a Plan field — so just verify that
		// having all tasks done with no open PRs satisfies requirements.
		r := CheckCompletionRequirements(p, []string{"t1"}, nil)
		require.True(t, r.Satisfied, "done_when must not prevent satisfaction")
	})
}
