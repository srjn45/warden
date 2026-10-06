package cli

import (
	"strings"
	"testing"
)

const planEndingBase = `"id":"plan-aa000001","project_id":"proj1","name":"flow","goal":"g","revision":3,
	"execution_mode":"autopilot","created_at":"2026-10-01T00:00:00Z","updated_at":"2026-10-01T00:00:00Z"`

const planInProgressFinalPRJSON = `{` + planEndingBase + `,"status":"in_progress",
	"outcome":{"integration_branch":"autopilot/flow","default_branch":"main",
	 "final_pr":{"number":77,"url":"https://github.com/o/r/pull/77","state":"open"}}}`

const planMergedJSON = `{` + planEndingBase + `,"status":"completed","completed_at":"2026-10-02T00:00:00Z",
	"task_summary":{"total":3,"done":2,"skipped":1},
	"outcome":{"integration_branch":"autopilot/flow","default_branch":"main",
	 "final_pr":{"number":77,"url":"https://github.com/o/r/pull/77","state":"merged"},
	 "branch_fate":"deleted","branch_deleted_at":"2026-10-02T00:01:00Z"},
	"execution_summary":{"plan_id":"plan-aa000001","plan_name":"flow","started_at":"2026-10-01T00:00:00Z",
	 "completed_at":"2026-10-01T01:30:00Z","tasks_total":3,"tasks_done":2,"outcome_note":"completed"},
	"branch_summaries":[{"name":"t1","pr":{"number":70,"state":"merged"}},{"name":"t2","pr":{"number":71,"state":"merged"}}],
	"cleanup_evidence":{"attempted_at":"2026-10-02T00:00:00Z","errors":["worktree busy"]}}`

const planLeftoverJSON = `{` + planEndingBase + `,"status":"completed","completed_at":"2026-10-02T00:00:00Z",
	"integration_branch_leftover":true,"integration_branch_leftover_commits":4,
	"outcome":{"integration_branch":"autopilot/flow","default_branch":"main","branch_fate":"leftover_unmerged"}}`

const planPipelineJSON = `{"id":"plan-aa000002","project_id":"proj1","name":"pipe","goal":"g","revision":2,
	"execution_mode":"pipeline","status":"completed","completed_at":"2026-10-02T00:00:00Z",
	"created_at":"2026-10-01T00:00:00Z","updated_at":"2026-10-01T00:00:00Z",
	"execution_summary":{"plan_id":"plan-aa000002","plan_name":"pipe","started_at":"2026-10-01T00:00:00Z",
	 "completed_at":"2026-10-01T00:10:00Z","tasks_total":1,"tasks_done":1}}`

func showPlan(t *testing.T, id, body string, extra ...string) string {
	t.Helper()
	addr := stubDaemon(t, routedDaemon(t, map[string]string{"GET /api/v1/plans/" + id: body}, nil, nil))
	out, err := runCLI(t, addr, append([]string{"plan", "show", id}, extra...)...)
	if err != nil {
		t.Fatalf("plan show %v: %v", extra, err)
	}
	return out
}

func requireAll(t *testing.T, out string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Fatalf("missing %q:\n%s", w, out)
		}
	}
}

func TestPlanShowEndingInProgressFinalPR(t *testing.T) {
	out := showPlan(t, "plan-aa000001", planInProgressFinalPRJSON)
	requireAll(t, out, "final_pr:       #77 (open) https://github.com/o/r/pull/77")
	if strings.Contains(out, "outcome:") {
		t.Fatalf("in_progress plan must not print the outcome block:\n%s", out)
	}
	requireAll(t, showPlan(t, "plan-aa000001", planInProgressFinalPRJSON, "--json"),
		`"final_pr"`, `"number": 77`, `"state": "open"`)
}

func TestPlanShowEndingCompletedMerged(t *testing.T) {
	out := showPlan(t, "plan-aa000001", planMergedJSON)
	requireAll(t, out, "outcome:", "final_pr:     #77 (merged)",
		"autopilot/flow — deleted 2026-10-02T00:01:00Z",
		"duration 1h30m0s, 2/3 tasks done, 1 skipped, 3 PRs landed",
		"cleanup_failure: worktree busy")
	if strings.Contains(out, "WARNING") {
		t.Fatalf("merged plan must not warn:\n%s", out)
	}
	requireAll(t, showPlan(t, "plan-aa000001", planMergedJSON, "--json"),
		`"execution_summary"`, `"tasks_done": 2`, `"branch_fate": "deleted"`, `"cleanup_evidence"`)
}

func TestPlanShowEndingLeftoverUnmerged(t *testing.T) {
	out := showPlan(t, "plan-aa000001", planLeftoverJSON)
	requireAll(t, out, "still exists with unmerged commits",
		"WARNING: integration branch autopilot/flow still has 4 commit(s) not on main")
	requireAll(t, showPlan(t, "plan-aa000001", planLeftoverJSON, "--json"),
		`"integration_branch_leftover": true`, `"integration_branch_leftover_commits": 4`)
}

func TestPlanShowEndingPipelineNoFinalPR(t *testing.T) {
	out := showPlan(t, "plan-aa000002", planPipelineJSON)
	requireAll(t, out, "outcome:", "final_pr:     none", "duration 10m0s, 1/1 tasks done")
	if strings.Contains(out, "branch:") || strings.Contains(out, "WARNING") {
		t.Fatalf("pipeline plan has no branch line or warning:\n%s", out)
	}
	requireAll(t, showPlan(t, "plan-aa000002", planPipelineJSON, "--json"), `"execution_summary"`)
}
