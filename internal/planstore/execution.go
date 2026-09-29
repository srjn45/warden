package planstore

import (
	"crypto/rand"
	"fmt"
	"time"
)

// ExecutionStatus is the runtime state of a PlanExecution.
type ExecutionStatus string

const (
	ExecutionStatusRunning   ExecutionStatus = "running"
	ExecutionStatusCompleted ExecutionStatus = "completed"
	ExecutionStatusFailed    ExecutionStatus = "failed"
	ExecutionStatusCancelled ExecutionStatus = "cancelled"
)

// PlanExecution is a single attempt to execute a Plan. Currently the fields are
// flattened onto the Plan record; this type names the concept so later tasks can
// promote it to a first-class ScrivaDB record without re-litigating vocabulary.
//
// A Plan may accumulate multiple PlanExecutions over its lifetime
// (Plan.ExecutionHistory). Only one is active at a time (Plan.ActiveExecution).
type PlanExecution struct {
	ID             string            `json:"id"` // pe-<8hex>
	PlanID         string            `json:"plan_id"`
	ExecutionMode  PlanExecutionMode `json:"execution_mode"`
	ExecutorID     string            `json:"executor_id,omitempty"` // autopilot_run_id | pipeline_id | orchestrator_id
	StartedAt      time.Time         `json:"started_at"`
	CompletedAt    *time.Time        `json:"completed_at,omitempty"`
	TerminalStatus ExecutionStatus   `json:"terminal_status,omitempty"`
	TaskProgress   map[string]string `json:"task_progress,omitempty"`
	PlanBranches   []string          `json:"plan_branches,omitempty"`
}

// ExecutionSummary is a compact read-only report produced when a PlanExecution
// completes or is archived. Generated on demand from a completed Plan; not
// stored as its own DB record.
type ExecutionSummary struct {
	PlanID        string            `json:"plan_id"`
	PlanName      string            `json:"plan_name"`
	Goal          string            `json:"goal"`
	ExecutionMode PlanExecutionMode `json:"execution_mode"`
	ExecutorID    string            `json:"executor_id,omitempty"`
	StartedAt     time.Time         `json:"started_at"`
	CompletedAt   *time.Time        `json:"completed_at,omitempty"`
	TasksTotal    int               `json:"tasks_total"`
	TasksDone     int               `json:"tasks_done"`
	OutcomeNote   string            `json:"outcome_note,omitempty"`
}

// PullRequestSummary records the GitHub PR evidence for a task branch.
type PullRequestSummary struct {
	URL      string     `json:"url"`
	Branch   string     `json:"branch"`
	Number   int        `json:"number,omitempty"`
	State    string     `json:"state"` // "open", "merged", "closed"
	Title    string     `json:"title,omitempty"`
	MergedAt *time.Time `json:"merged_at,omitempty"`
}

// BranchSummary associates a git branch with the task and agent that created it
// and records any PR opened against it.
type BranchSummary struct {
	Name    string              `json:"name"`
	TaskID  string              `json:"task_id,omitempty"`
	AgentID string              `json:"agent_id,omitempty"`
	PR      *PullRequestSummary `json:"pr,omitempty"`
}

// TaskOutcome records durable evidence for a single task's terminal result.
// This state lives on the Plan, not on the worker Agent, so it survives Agent
// teardown. It captures assignment, verified checks, PRs, and final status.
type TaskOutcome struct {
	TaskID         string               `json:"task_id"`
	Status         string               `json:"status"` // "done", "skipped", "failed"
	AssignedAgent  string               `json:"assigned_agent,omitempty"`
	Branch         string               `json:"branch,omitempty"`
	PullRequests   []PullRequestSummary `json:"pull_requests,omitempty"`
	VerifiedChecks []string             `json:"verified_checks,omitempty"`
	CompletedAt    time.Time            `json:"completed_at"`
	Note           string               `json:"note,omitempty"`
}

// CompletionRequirements captures the two daemon-enforced gates (spec Rule 5)
// that must pass before a plan may transition in_progress → completed.
// CheckCompletionRequirements populates this from a Plan snapshot; it performs
// no I/O and does not evaluate done_when text (which is human-readable guidance
// only).
type CompletionRequirements struct {
	AllTasksDone   bool     `json:"all_tasks_done"`
	NoOpenPRs      bool     `json:"no_open_prs"`
	Satisfied      bool     `json:"satisfied"`
	PendingTaskIDs []string `json:"pending_task_ids,omitempty"`
	OpenPRBranches []string `json:"open_pr_branches,omitempty"`
}

// NewPlanExecutionID returns a fresh pe-<8hex> execution identifier.
func NewPlanExecutionID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("planstore: crypto/rand unavailable: " + err.Error())
	}
	return fmt.Sprintf("pe-%x", b)
}

// CheckCompletionRequirements returns the gate state for p given the complete
// set of task IDs declared in the plan YAML (taskIDs) and the branches that
// still carry an open PR (openPRBranches). Callers resolve open-PR state before
// calling; this function is pure (no I/O).
//
// Rule 5: done_when text is never evaluated here — it is human-readable
// guidance for the operator, not a machine-executable gate.
func CheckCompletionRequirements(p *Plan, taskIDs []string, openPRBranches []string) CompletionRequirements {
	var pending []string
	for _, id := range taskIDs {
		if id == "" {
			continue
		}
		st := ""
		if p.TaskProgress != nil {
			st = p.TaskProgress[id]
		}
		if st != "done" && st != "skipped" {
			pending = append(pending, id)
		}
	}
	allDone := len(pending) == 0
	noPRs := len(openPRBranches) == 0
	return CompletionRequirements{
		AllTasksDone:   allDone,
		NoOpenPRs:      noPRs,
		Satisfied:      allDone && noPRs,
		PendingTaskIDs: pending,
		OpenPRBranches: openPRBranches,
	}
}
