package planstore

import (
	"crypto/rand"
	"fmt"
	"sort"
	"strings"
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

// ExecutionSnapshot is the immutable copy of a Plan's canonical definition
// captured when a PlanExecution starts (design freeze §7 snapshot-at-start).
// Task spawning, progress gates, and completion enumeration for that execution
// MUST use this snapshot — not live Plan definition fields and not repository
// YAML. Structural edits while status=in_progress are rejected by PlanService.Update
// (pending-only); they never mutate an in-flight snapshot.
type ExecutionSnapshot struct {
	Revision    int64      `json:"revision"`
	ContentHash string     `json:"content_hash,omitempty"`
	Name        string     `json:"name"`
	Goal        string     `json:"goal"`
	Constraints []string   `json:"constraints,omitempty"`
	DoneWhen    []string   `json:"done_when,omitempty"`
	Tasks       []PlanTask `json:"tasks,omitempty"`
}

// SnapshotFromPlan builds an ExecutionSnapshot from the Plan's current canonical
// definition. Callers attach it to PlanExecution at run start.
func SnapshotFromPlan(p *Plan) *ExecutionSnapshot {
	if p == nil {
		return nil
	}
	hash := p.ContentHash
	if hash == "" {
		hash = ComputeContentHash(p)
	}
	tasks := make([]PlanTask, 0, len(p.Tasks))
	for _, t := range p.Tasks {
		tasks = append(tasks, PlanTask{
			ID:     t.ID,
			Prompt: t.Prompt,
			After:  append([]string(nil), t.After...),
		})
	}
	return &ExecutionSnapshot{
		Revision:    p.Revision,
		ContentHash: hash,
		Name:        p.Name,
		Goal:        p.Goal,
		Constraints: append([]string(nil), p.Constraints...),
		DoneWhen:    append([]string(nil), p.DoneWhen...),
		Tasks:       tasks,
	}
}

// TaskIDs returns the ordered task IDs from the snapshot.
func (s *ExecutionSnapshot) TaskIDs() []string {
	if s == nil || len(s.Tasks) == 0 {
		return nil
	}
	out := make([]string, 0, len(s.Tasks))
	for _, t := range s.Tasks {
		if id := strings.TrimSpace(t.ID); id != "" {
			out = append(out, id)
		}
	}
	return out
}

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
	// Snapshot is the immutable definition+revision captured at execution start
	// (docs/specs/2026-09-30-scrivadb-canonical-plans.md §7). Nil on pre-cutover
	// executions that started before snapshot-at-start shipped.
	Snapshot *ExecutionSnapshot `json:"snapshot,omitempty"`
	// TaskJobMap records the plan-task → pipeline-job mapping for pipeline-mode
	// executions. The adapter stores this on Plan execution evidence (not a
	// second mutable task ledger) so the mapping survives pipeline teardown.
	// Convention: job IDs equal the stable canonical task IDs (identity mapping).
	TaskJobMap map[string]string `json:"task_job_map,omitempty"`
}

// ExecutionSummary is a compact read-only report produced when a PlanExecution
// is finalized. Persist once on Plan.ExecutionSummary before executor cleanup;
// never overwrite once set (spec: never lose a completed summary).
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

// CleanupEvidence records a partial disposable-executor teardown during
// Finalize. While present (and Failed), the Plan stays in_progress so cleanup
// can be retried. PR refs, events, summaries, and global audit are never
// cleared by cleanup.
type CleanupEvidence struct {
	AttemptedAt    time.Time `json:"attempted_at"`
	Errors         []string  `json:"errors,omitempty"`
	DeletedIDs     []string  `json:"deleted_ids,omitempty"`
	PendingIDs     []string  `json:"pending_ids,omitempty"`
	WorktreeErrors []string  `json:"worktree_errors,omitempty"`
}

// Failed reports whether cleanup left work unfinished.
func (e *CleanupEvidence) Failed() bool {
	if e == nil {
		return false
	}
	return len(e.Errors) > 0 || len(e.PendingIDs) > 0 || len(e.WorktreeErrors) > 0
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

// CompletionRequirements captures the machine-verifiable gates that must pass
// before a plan may transition in_progress → completed. The struct is populated
// by CheckCompletionRequirements (two rule-5 gates only) or by
// EvalCompletionFromEvents (all five gates from typed event evidence).
// Neither function evaluates done_when text — that field is human-readable
// guidance for the operator, never a machine-executable gate (spec Rule 5).
type CompletionRequirements struct {
	// Tier 1: rule-5 daemon gates (always evaluated).
	AllTasksDone bool `json:"all_tasks_done"`
	NoOpenPRs    bool `json:"no_open_prs"`

	// Tier 2: event-derived evidence gates.
	// ChecksPassing is true when all required named checks have a check_completed
	// event. Trivially true when no required_checks are declared in the plan YAML.
	ChecksPassing bool `json:"checks_passing"`
	// NoLiveAgents is true when the execution is either not started or has reached
	// a terminal state (completion_verified / execution_failed / execution_stopped).
	NoLiveAgents bool `json:"no_live_agents"`
	// ResourcesClean is true when every branch that received a branch_pushed event
	// also has a corresponding worktree_removed event.
	ResourcesClean bool `json:"resources_clean"`

	// Aggregate: true only when all five gates are satisfied.
	Satisfied bool `json:"satisfied"`

	// Unmet-requirement details — a structured list of exactly what is missing.
	PendingTaskIDs  []string `json:"pending_task_ids,omitempty"`  // task IDs not yet terminal
	OpenPRBranches  []string `json:"open_pr_branches,omitempty"`  // branches with open PRs
	MissingChecks   []string `json:"missing_checks,omitempty"`    // required checks without evidence
	LiveExecutorIDs []string `json:"live_executor_ids,omitempty"` // executor / agent IDs still running
	UncleanBranches []string `json:"unclean_branches,omitempty"`  // branches whose worktrees remain
}

// NewPlanExecutionID returns a fresh pe-<8hex> execution identifier.
func NewPlanExecutionID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("planstore: crypto/rand unavailable: " + err.Error())
	}
	return fmt.Sprintf("pe-%x", b)
}

// CheckCompletionRequirements returns the rule-5 gate state (tasks done, no
// open PRs) for p. It does not evaluate the event-derived gates; those default
// to trivially satisfied (true). Callers that need full five-gate evaluation
// should use EvalCompletionFromEvents instead.
//
// Rule 5: done_when text is never evaluated — it is human-readable guidance
// for the operator, not a machine-executable gate.
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
		ChecksPassing:  true, // not evaluated by this function
		NoLiveAgents:   true, // not evaluated by this function
		ResourcesClean: true, // not evaluated by this function
		Satisfied:      allDone && noPRs,
		PendingTaskIDs: pending,
		OpenPRBranches: openPRBranches,
	}
}

// EvalCompletionFromEvents returns the full five-gate CompletionRequirements for
// plan p, deriving evidence from typed PlanExecutionEvents via the event log.
//
// A task is terminal when Plan.TaskProgress records it as "done"/"skipped" OR
// when a task_evidence_verified event exists for its ID. The OR logic lets
// manual plans (no events) continue to use TaskProgress, while automated
// executions can prove completion without agent prose.
//
// The openPRBranches argument must be pre-resolved by the caller (typically via
// a live gh pr list query). This function is otherwise pure (no I/O).
//
// Rule 5 guarantee: done_when text is never evaluated here.
func EvalCompletionFromEvents(
	plan *Plan,
	taskIDs []string,
	openPRBranches []string,
	events []*PlanExecutionEvent,
) CompletionRequirements {
	// Build evidence maps from the event log.
	evidencedTaskIDs := map[string]bool{}
	executionStarted := false
	executionTerminal := false
	pushedBranches := map[string]bool{}
	removedBranches := map[string]bool{}

	for _, ev := range events {
		p := ev.Payload
		switch ev.Kind {
		case EventKindTaskEvidenceVerified:
			if p != nil && p.TaskID != "" {
				evidencedTaskIDs[p.TaskID] = true
			}
		case EventKindExecutionStarted:
			executionStarted = true
		case EventKindCompletionVerified, EventKindExecutionFailed, EventKindExecutionStopped:
			executionTerminal = true
		case EventKindBranchPushed:
			if p != nil && p.Branch != "" {
				pushedBranches[p.Branch] = true
			}
		case EventKindWorktreeRemoved:
			if p != nil && p.Branch != "" {
				removedBranches[p.Branch] = true
			}
		}
	}

	// 1. AllTasksDone: terminal if TaskProgress says done/skipped OR event-evidenced.
	var pendingTasks []string
	for _, id := range taskIDs {
		if id == "" {
			continue
		}
		done := false
		if plan.TaskProgress != nil {
			st := plan.TaskProgress[id]
			if st == "done" || st == "skipped" {
				done = true
			}
		}
		if !done && evidencedTaskIDs[id] {
			done = true
		}
		if !done {
			pendingTasks = append(pendingTasks, id)
		}
	}
	sort.Strings(pendingTasks)

	// 2. ChecksPassing: trivially satisfied until required_checks YAML field exists.
	checksPassing := true

	// 3. NoLiveAgents: satisfied when no execution was started, or the execution
	// reached a terminal state (completion_verified, execution_failed, execution_stopped).
	noLiveAgents := !executionStarted || executionTerminal
	var liveExecutorIDs []string
	if !noLiveAgents && plan.ActiveExecution != nil && plan.ActiveExecution.ExecutorID != "" {
		liveExecutorIDs = []string{plan.ActiveExecution.ExecutorID}
	}

	// 4. ResourcesClean: every branch that was pushed must have a worktree removed.
	var uncleanBranches []string
	for branch := range pushedBranches {
		if !removedBranches[branch] {
			uncleanBranches = append(uncleanBranches, branch)
		}
	}
	sort.Strings(uncleanBranches)
	resourcesClean := len(uncleanBranches) == 0

	allDone := len(pendingTasks) == 0
	noPRs := len(openPRBranches) == 0
	satisfied := allDone && noPRs && checksPassing && noLiveAgents && resourcesClean

	return CompletionRequirements{
		AllTasksDone:    allDone,
		NoOpenPRs:       noPRs,
		ChecksPassing:   checksPassing,
		NoLiveAgents:    noLiveAgents,
		ResourcesClean:  resourcesClean,
		Satisfied:       satisfied,
		PendingTaskIDs:  pendingTasks,
		OpenPRBranches:  openPRBranches,
		LiveExecutorIDs: liveExecutorIDs,
		UncleanBranches: uncleanBranches,
	}
}
