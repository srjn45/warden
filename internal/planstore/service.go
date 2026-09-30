package planstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

var (
	// ErrNotPending is returned when Update is called on a non-pending plan.
	ErrNotPending = errors.New("plan is not pending")
	// ErrInvalidTransition is returned when a status change is not allowed.
	ErrInvalidTransition = errors.New("invalid plan status transition")
	// ErrTasksIncomplete is returned when completing a plan whose tasks are not all done/skipped.
	ErrTasksIncomplete = errors.New("plan tasks are incomplete")
	// ErrBranchesUnmerged is returned when completing a plan that still has open PRs.
	ErrBranchesUnmerged = errors.New("plan branches are unmerged")
	// ErrInvalidTaskStatus is returned when UpdateTaskStatus is given an unknown status.
	ErrInvalidTaskStatus = errors.New("invalid task status")
)

// ValidationError is a field-level request validation failure.
type ValidationError struct {
	Field string
	Msg   string
}

func (e *ValidationError) Error() string {
	if e == nil {
		return "invalid plan"
	}
	if e.Field == "" {
		return e.Msg
	}
	return e.Field + ": " + e.Msg
}

// TasksIncompleteError lists YAML task IDs that are not done or skipped.
type TasksIncompleteError struct {
	TaskIDs []string
}

func (e *TasksIncompleteError) Error() string {
	if e == nil {
		return ErrTasksIncomplete.Error()
	}
	return "incomplete tasks: " + strings.Join(e.TaskIDs, ", ")
}

func (e *TasksIncompleteError) Unwrap() error { return ErrTasksIncomplete }

// BranchesUnmergedError lists plan branches that still have an open PR.
type BranchesUnmergedError struct {
	Branches []string
}

func (e *BranchesUnmergedError) Error() string {
	if e == nil {
		return ErrBranchesUnmerged.Error()
	}
	return "unmerged branches: " + strings.Join(e.Branches, ", ")
}

func (e *BranchesUnmergedError) Unwrap() error { return ErrBranchesUnmerged }

// UnmetRequirementsError is returned by PlanService.Transition when a plan
// cannot move to completed. It carries a structured snapshot of every unmet
// gate so callers can surface exactly what is missing without parsing text.
//
// It satisfies errors.Is / errors.As for the legacy ErrTasksIncomplete and
// ErrBranchesUnmerged sentinels via a multi-error Unwrap chain, so existing
// call sites do not need to be updated.
type UnmetRequirementsError struct {
	Requirements CompletionRequirements
}

func (e *UnmetRequirementsError) Error() string {
	var parts []string
	if !e.Requirements.AllTasksDone {
		if len(e.Requirements.PendingTaskIDs) > 0 {
			parts = append(parts, "incomplete tasks: "+strings.Join(e.Requirements.PendingTaskIDs, ", "))
		} else {
			parts = append(parts, "incomplete tasks")
		}
	}
	if !e.Requirements.NoOpenPRs {
		if len(e.Requirements.OpenPRBranches) > 0 {
			parts = append(parts, "unmerged branches: "+strings.Join(e.Requirements.OpenPRBranches, ", "))
		} else {
			parts = append(parts, "unmerged branches")
		}
	}
	if !e.Requirements.NoLiveAgents {
		if len(e.Requirements.LiveExecutorIDs) > 0 {
			parts = append(parts, "live executors: "+strings.Join(e.Requirements.LiveExecutorIDs, ", "))
		} else {
			parts = append(parts, "live executors")
		}
	}
	if !e.Requirements.ResourcesClean {
		if len(e.Requirements.UncleanBranches) > 0 {
			parts = append(parts, "unclean worktrees: "+strings.Join(e.Requirements.UncleanBranches, ", "))
		} else {
			parts = append(parts, "unclean worktrees")
		}
	}
	if len(parts) == 0 {
		return "completion requirements not satisfied"
	}
	return "unmet requirements: " + strings.Join(parts, "; ")
}

// Unwrap returns the specific legacy errors (ErrTasksIncomplete,
// ErrBranchesUnmerged) so that errors.Is / errors.As traversals continue to
// work after the transition to the unified UnmetRequirementsError.
func (e *UnmetRequirementsError) Unwrap() []error {
	var errs []error
	if !e.Requirements.AllTasksDone {
		errs = append(errs, &TasksIncompleteError{TaskIDs: e.Requirements.PendingTaskIDs})
	}
	if !e.Requirements.NoOpenPRs {
		errs = append(errs, &BranchesUnmergedError{Branches: e.Requirements.OpenPRBranches})
	}
	return errs
}

// InvalidTransitionError names a forbidden from→to status change.
type InvalidTransitionError struct {
	From PlanStatus
	To   PlanStatus
}

func (e *InvalidTransitionError) Error() string {
	if e == nil {
		return ErrInvalidTransition.Error()
	}
	return fmt.Sprintf("cannot transition from %s to %s", e.From, e.To)
}

func (e *InvalidTransitionError) Unwrap() error { return ErrInvalidTransition }

// TaskSpec is one task in a create/update request.
type TaskSpec struct {
	ID     string
	Prompt string
	After  []string
}

// CreateRequest is the body of PlanService.Create.
type CreateRequest struct {
	Name        string
	Goal        string
	Tasks       []TaskSpec
	Constraints []string
	DoneWhen    []string
}

// UpdateRequest is a partial replacement of editable plan definition fields.
// Nil pointer fields are left unchanged.
//
// ExpectedRevision, when non-nil, is the optimistic-concurrency token passed to
// UpdateIf. When nil, the service uses the revision observed at the start of
// Update (still subject to conflict if another writer races).
type UpdateRequest struct {
	Name             *string
	Goal             *string
	Tasks            *[]TaskSpec
	Constraints      *[]string
	DoneWhen         *[]string
	ExpectedRevision *int64
}

// TransitionOptions carries optional side data for a status change.
type TransitionOptions struct {
	ExecutionMode    PlanExecutionMode
	ExpectedRevision *int64
}

// CommandRunner executes an external command in an optional working directory.
// It is the seam mocked in tests for git/gh.
type CommandRunner interface {
	Run(ctx context.Context, dir, name string, args ...string) (string, error)
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		msg := string(out)
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			msg = strings.TrimSpace(msg + " " + string(ee.Stderr))
		}
		return msg, err
	}
	return string(out), nil
}

// PlanService wraps Store with validation and lifecycle transitions over
// canonical ScrivaDB Plan records. Normal create/update/transition/finalize
// paths never write or move repository YAML (see legacy_yaml.go).
type PlanService struct {
	store       *Store
	projectRoot func(projectID string) string
	runner      CommandRunner
	now         func() time.Time
}

// ServiceOption configures a PlanService.
type ServiceOption func(*PlanService)

// WithProjectRoot sets how a project id is resolved to a filesystem root.
// The default treats projectID as the root path (local-project convention).
func WithProjectRoot(fn func(projectID string) string) ServiceOption {
	return func(s *PlanService) {
		if fn != nil {
			s.projectRoot = fn
		}
	}
}

// WithRunner replaces the git/gh command runner (tests).
func WithRunner(r CommandRunner) ServiceOption {
	return func(s *PlanService) {
		if r != nil {
			s.runner = r
		}
	}
}

// WithNow replaces the clock used for started_at/completed_at (tests).
func WithNow(fn func() time.Time) ServiceOption {
	return func(s *PlanService) {
		if fn != nil {
			s.now = fn
		}
	}
}

// NewPlanService returns a service wrapping store.
func NewPlanService(store *Store, opts ...ServiceOption) *PlanService {
	s := &PlanService{
		store:       store,
		projectRoot: func(id string) string { return id },
		runner:      execRunner{},
		now:         time.Now,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func (s *PlanService) root(projectID string) (string, error) {
	root := ""
	if s.projectRoot != nil {
		root = s.projectRoot(projectID)
	}
	if strings.TrimSpace(root) == "" {
		return "", &ValidationError{Field: "project_id", Msg: "project root could not be resolved"}
	}
	return root, nil
}

func (s *PlanService) run(ctx context.Context, dir, name string, args ...string) (string, error) {
	if s.runner == nil {
		return execRunner{}.Run(ctx, dir, name, args...)
	}
	return s.runner.Run(ctx, dir, name, args...)
}

func (s *PlanService) clock() time.Time {
	if s.now == nil {
		return time.Now().UTC()
	}
	return s.now().UTC()
}

// Get returns the plan with the given ID.
func (s *PlanService) Get(ctx context.Context, planID string) (*Plan, error) {
	return s.store.Get(ctx, planID)
}

// List returns plans for projectID, optionally filtered by status (empty = all).
func (s *PlanService) List(ctx context.Context, projectID string, status PlanStatus) ([]*Plan, error) {
	if status == "" {
		return s.store.ListByProject(ctx, projectID)
	}
	return s.store.ListByProjectAndStatus(ctx, projectID, status)
}

// Create validates req and inserts a canonical ScrivaDB Plan (revision=1).
// No repository files are written; a plans/ directory is not required.
func (s *PlanService) Create(ctx context.Context, projectID string, req CreateRequest) (*Plan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return nil, &ValidationError{Field: "name", Msg: "name is required"}
	}
	if err := validateTasks(req.Tasks); err != nil {
		return nil, err
	}

	tasks := taskSpecsToPlanTasks(req.Tasks)
	progress := make(map[string]string, len(tasks))
	for _, t := range tasks {
		progress[t.ID] = "pending"
	}
	p := &Plan{
		ID:           PlanID(projectID, name),
		ProjectID:    projectID,
		Name:         name,
		Goal:         req.Goal,
		Constraints:  append([]string(nil), req.Constraints...),
		DoneWhen:     append([]string(nil), req.DoneWhen...),
		Tasks:        tasks,
		Status:       PlanStatusPending,
		TaskProgress: progress,
	}
	if err := s.store.Create(ctx, p); err != nil {
		return nil, err
	}
	return s.store.Get(ctx, p.ID)
}

// Update rejects non-pending plans, validates fields being updated, and applies
// the definition change atomically via UpdateIf (revision bump + content hash).
func (s *PlanService) Update(ctx context.Context, planID string, req UpdateRequest) (*Plan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p, err := s.store.Get(ctx, planID)
	if err != nil {
		return nil, err
	}
	if p.Status != PlanStatusPending {
		return nil, ErrNotPending
	}

	if req.Name != nil {
		if strings.TrimSpace(*req.Name) == "" {
			return nil, &ValidationError{Field: "name", Msg: "name is required"}
		}
	}
	if req.Tasks != nil {
		if err := validateTasks(*req.Tasks); err != nil {
			return nil, err
		}
	}

	expected := p.Revision
	if req.ExpectedRevision != nil {
		expected = *req.ExpectedRevision
	}

	if err := s.store.UpdateIf(ctx, planID, expected, func(pl *Plan) error {
		if pl.Status != PlanStatusPending {
			return ErrNotPending
		}
		if req.Name != nil {
			pl.Name = strings.TrimSpace(*req.Name)
		}
		if req.Goal != nil {
			pl.Goal = *req.Goal
		}
		if req.Constraints != nil {
			pl.Constraints = append([]string(nil), (*req.Constraints)...)
		}
		if req.DoneWhen != nil {
			pl.DoneWhen = append([]string(nil), (*req.DoneWhen)...)
		}
		if req.Tasks != nil {
			pl.Tasks = taskSpecsToPlanTasks(*req.Tasks)
			progress := make(map[string]string, len(pl.Tasks))
			for _, t := range pl.Tasks {
				if prev, ok := pl.TaskProgress[t.ID]; ok {
					progress[t.ID] = prev
				} else {
					progress[t.ID] = "pending"
				}
			}
			pl.TaskProgress = progress
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return s.store.Get(ctx, planID)
}

// Transition enforces the plan status state machine, optionally gating
// in_progress → completed on tasks and merged branches, then updates Status
// (and timestamps) atomically via UpdateIf. No repository files are moved.
func (s *PlanService) Transition(ctx context.Context, planID string, to PlanStatus, opts TransitionOptions) (*Plan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !to.Valid() {
		return nil, &InvalidTransitionError{To: to}
	}
	p, err := s.store.Get(ctx, planID)
	if err != nil {
		return nil, err
	}
	if !canTransition(p.Status, to) {
		return nil, &InvalidTransitionError{From: p.Status, To: to}
	}
	if p.Status == PlanStatusInProgress && to == PlanStatusCompleted {
		if err := s.evaluateCompletion(ctx, p); err != nil {
			return nil, err
		}
	}

	expected := p.Revision
	if opts.ExpectedRevision != nil {
		expected = *opts.ExpectedRevision
	}
	now := s.clock()
	if err := s.store.UpdateIf(ctx, planID, expected, func(pl *Plan) error {
		if !canTransition(pl.Status, to) {
			return &InvalidTransitionError{From: pl.Status, To: to}
		}
		pl.Status = to
		if to == PlanStatusInProgress {
			if opts.ExecutionMode != "" {
				pl.ExecutionMode = opts.ExecutionMode
			}
			if pl.StartedAt == nil {
				ts := now
				pl.StartedAt = &ts
			}
		}
		if to == PlanStatusCompleted && pl.CompletedAt == nil {
			ts := now
			pl.CompletedAt = &ts
		}
		if to == PlanStatusArchived && pl.ArchivedAt == nil {
			ts := now
			pl.ArchivedAt = &ts
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return s.store.Get(ctx, planID)
}

// UpdateTaskStatus merges taskID→status into plan.TaskProgress.
func (s *PlanService) UpdateTaskStatus(ctx context.Context, planID, taskID, status string) (*Plan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return nil, &ValidationError{Field: "task_id", Msg: "task id is required"}
	}
	status = strings.ToLower(strings.TrimSpace(status))
	switch status {
	case "pending", "in_progress", "done", "skipped":
	default:
		return nil, fmt.Errorf("%w: %s", ErrInvalidTaskStatus, status)
	}
	if err := s.store.Update(ctx, planID, func(p *Plan) error {
		if p.TaskProgress == nil {
			p.TaskProgress = map[string]string{}
		}
		p.TaskProgress[taskID] = status
		return nil
	}); err != nil {
		return nil, err
	}
	return s.store.Get(ctx, planID)
}

// evaluateCompletion checks all machine-verifiable completion gates for a plan
// transitioning from in_progress → completed. It derives task and PR evidence
// from canonical Plan.Tasks, TaskProgress, and typed PlanExecutionEvents; it
// never evaluates done_when text (spec Rule 5) and never reads repository YAML.
// Returns an UnmetRequirementsError carrying a structured list of every unmet
// gate, or nil when all gates are satisfied.
func (s *PlanService) evaluateCompletion(ctx context.Context, p *Plan) error {
	taskIDs := canonicalTaskIDs(p)

	// Resolve open PR branches (live I/O — kept outside the pure eval function).
	var openPRBranches []string
	if len(p.Branches) > 0 {
		root, err := s.root(p.ProjectID)
		if err != nil {
			return err
		}
		openPRBranches, err = s.resolveOpenPRBranches(ctx, p, root)
		if err != nil {
			return err
		}
	}

	// Load execution events for the active execution (if any).
	var events []*PlanExecutionEvent
	if p.ActiveExecution != nil && p.ActiveExecution.ID != "" {
		var err error
		events, err = s.store.ListEvents(ctx, p.ID, p.ActiveExecution.ID)
		if err != nil {
			return fmt.Errorf("planstore: list events for completion check: %w", err)
		}
	}

	reqs := EvalCompletionFromEvents(p, taskIDs, openPRBranches, events)
	if reqs.Satisfied {
		return nil
	}
	return &UnmetRequirementsError{Requirements: reqs}
}

// resolveOpenPRBranches returns the subset of plan.Branches that still carry
// an open GitHub PR. Returns nil when there are no branches to check.
func (s *PlanService) resolveOpenPRBranches(ctx context.Context, p *Plan, root string) ([]string, error) {
	if len(p.Branches) == 0 {
		return nil, nil
	}
	var unmerged []string
	for _, branch := range p.Branches {
		branch = strings.TrimSpace(branch)
		if branch == "" {
			continue
		}
		out, err := s.run(ctx, root, "gh", "pr", "list", "--head", branch, "--state", "open", "--json", "number")
		if err != nil {
			return nil, fmt.Errorf("gh pr list --head %s: %w (%s)", branch, err, strings.TrimSpace(out))
		}
		if hasOpenPR(out) {
			unmerged = append(unmerged, branch)
		}
	}
	return unmerged, nil
}

// CleanupWorktrees removes worktrees, local branches, and remote branches for
// each entry in plan.Branches. Called after a successful complete transition.
func (s *PlanService) CleanupWorktrees(ctx context.Context, plan *Plan) error {
	if plan == nil || len(plan.Branches) == 0 {
		return nil
	}
	root, err := s.root(plan.ProjectID)
	if err != nil {
		return err
	}
	porcelain, _ := s.run(ctx, root, "git", "worktree", "list", "--porcelain")
	worktrees := parseWorktreeList(porcelain)

	var errs []error
	for _, branch := range plan.Branches {
		branch = strings.TrimSpace(branch)
		if branch == "" {
			continue
		}
		for _, wt := range worktrees {
			if wt.branch != branch {
				continue
			}
			if wt.path == "" || samePath(wt.path, root) {
				continue
			}
			if out, err := s.run(ctx, root, "git", "worktree", "remove", wt.path); err != nil {
				errs = append(errs, fmt.Errorf("git worktree remove %s: %w (%s)", wt.path, err, strings.TrimSpace(out)))
			}
		}
		if out, err := s.run(ctx, root, "git", "branch", "-d", branch); err != nil {
			errs = append(errs, fmt.Errorf("git branch -d %s: %w (%s)", branch, err, strings.TrimSpace(out)))
		}
		if out, err := s.run(ctx, root, "git", "push", "origin", "--delete", branch); err != nil {
			errs = append(errs, fmt.Errorf("git push origin --delete %s: %w (%s)", branch, err, strings.TrimSpace(out)))
		}
	}
	return errors.Join(errs...)
}

func canTransition(from, to PlanStatus) bool {
	switch from {
	case PlanStatusPending:
		return to == PlanStatusInProgress || to == PlanStatusArchived
	case PlanStatusInProgress:
		return to == PlanStatusPending || to == PlanStatusCompleted || to == PlanStatusArchived
	case PlanStatusCompleted:
		return to == PlanStatusArchived
	default:
		return false
	}
}

func validateTasks(tasks []TaskSpec) error {
	if len(tasks) == 0 {
		return &ValidationError{Field: "tasks", Msg: "at least one task is required"}
	}
	ids := make(map[string]struct{}, len(tasks))
	for i, t := range tasks {
		id := strings.TrimSpace(t.ID)
		prompt := strings.TrimSpace(t.Prompt)
		if id == "" {
			return &ValidationError{Field: "tasks", Msg: fmt.Sprintf("task[%d] is missing id", i)}
		}
		if prompt == "" {
			return &ValidationError{Field: "tasks", Msg: fmt.Sprintf("task %q is missing prompt", id)}
		}
		if _, dup := ids[id]; dup {
			return &ValidationError{Field: "tasks", Msg: fmt.Sprintf("duplicate task id %q", id)}
		}
		ids[id] = struct{}{}
	}
	for _, t := range tasks {
		id := strings.TrimSpace(t.ID)
		for _, dep := range t.After {
			dep = strings.TrimSpace(dep)
			if dep == "" {
				continue
			}
			if _, ok := ids[dep]; !ok {
				return &ValidationError{Field: "tasks", Msg: fmt.Sprintf("task %q after-ref %q does not exist", id, dep)}
			}
		}
	}
	return nil
}

func taskSpecsToPlanTasks(tasks []TaskSpec) []PlanTask {
	out := make([]PlanTask, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, PlanTask{
			ID:     strings.TrimSpace(t.ID),
			Prompt: t.Prompt,
			After:  append([]string(nil), t.After...),
		})
	}
	return out
}

// canonicalTaskIDs returns the task IDs that gate completion for p. When an
// ActiveExecution carries a snapshot-at-start, those IDs are authoritative
// (frozen design §7); otherwise the live Plan.Tasks definition is used.
func canonicalTaskIDs(p *Plan) []string {
	if p == nil {
		return nil
	}
	if p.ActiveExecution != nil && p.ActiveExecution.Snapshot != nil {
		if ids := p.ActiveExecution.Snapshot.TaskIDs(); len(ids) > 0 {
			return ids
		}
	}
	if len(p.Tasks) == 0 {
		return nil
	}
	out := make([]string, 0, len(p.Tasks))
	for _, t := range p.Tasks {
		if id := strings.TrimSpace(t.ID); id != "" {
			out = append(out, id)
		}
	}
	return out
}

func planSlug(name string) string {
	slug := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(name), " ", "-"))
	slug = filepath.Base(slug)
	return slug
}

func planFilename(name string) string {
	return planSlug(name) + ".yaml"
}

func hasOpenPR(output string) bool {
	s := strings.TrimSpace(output)
	if s == "" || s == "[]" {
		return false
	}
	var prs []struct {
		Number int `json:"number"`
	}
	if err := json.Unmarshal([]byte(s), &prs); err == nil {
		return len(prs) > 0
	}
	return true
}

type worktreeInfo struct {
	path   string
	branch string
}

func parseWorktreeList(out string) []worktreeInfo {
	var entries []worktreeInfo
	var cur *worktreeInfo
	flush := func() {
		if cur != nil {
			entries = append(entries, *cur)
			cur = nil
		}
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "worktree "):
			flush()
			cur = &worktreeInfo{path: strings.TrimPrefix(line, "worktree ")}
		case cur == nil:
			continue
		case strings.HasPrefix(line, "branch "):
			cur.branch = strings.TrimPrefix(strings.TrimPrefix(line, "branch "), "refs/heads/")
		}
	}
	flush()
	return entries
}

func samePath(a, b string) bool {
	if a == b {
		return true
	}
	aa, errA := filepath.Abs(a)
	bb, errB := filepath.Abs(b)
	if errA != nil || errB != nil {
		return false
	}
	return aa == bb
}
