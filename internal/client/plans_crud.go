package client

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/planbackup"
	"github.com/srjn45/warden/internal/planstore"
	"github.com/srjn45/warden/internal/plansync"
	"gopkg.in/yaml.v3"
)

type PlanSyncRequest struct {
	PlanID     string                 `json:"plan_id,omitempty"`
	Scope      plansync.Scope         `json:"scope"`
	Statuses   []planstore.PlanStatus `json:"statuses,omitempty"`
	Visibility plansync.Visibility    `json:"visibility,omitempty"`
	OwnerID    string                 `json:"owner_id,omitempty"`
}
type PlanSyncEnvelopes struct {
	Envelopes []plansync.Envelope `json:"envelopes"`
}

func (c *Client) PlansSyncPush(ctx context.Context, req PlanSyncRequest) (*PlanView, error) {
	var out PlanView
	if err := c.do(ctx, http.MethodPost, "/plans/sync/push", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
func (c *Client) PlansSyncPull(ctx context.Context, req PlanSyncRequest) (*PlanSyncEnvelopes, error) {
	var out PlanSyncEnvelopes
	if err := c.do(ctx, http.MethodPost, "/plans/sync/pull", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
func (c *Client) PlansSyncDiscover(ctx context.Context, req PlanSyncRequest) (*PlanSyncEnvelopes, error) {
	var out PlanSyncEnvelopes
	if err := c.do(ctx, http.MethodPost, "/plans/sync/discover", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PlanTaskSpec is one task in a create/update request or a hydrated Plan.
type PlanTaskSpec struct {
	ID     string   `json:"id"`
	Prompt string   `json:"prompt"`
	After  []string `json:"after,omitempty"`
}

// PlanTaskSummary is a computed task-progress rollup on Plan list/detail.
type PlanTaskSummary struct {
	Total      int `json:"total"`
	Done       int `json:"done"`
	InProgress int `json:"in_progress"`
	Pending    int `json:"pending"`
	Skipped    int `json:"skipped"`
}

// PlanRepoExport is last-export metadata for an optional repository replica.
type PlanRepoExport struct {
	SchemaVersion int        `json:"schema_version,omitempty"`
	Revision      int64      `json:"revision,omitempty"`
	ContentHash   string     `json:"content_hash,omitempty"`
	ExportedAt    *time.Time `json:"exported_at,omitempty"`
	Lifecycle     string     `json:"lifecycle,omitempty"`
	FilePath      string     `json:"file_path,omitempty"`
}

// RelatedPlanHit is one heuristic overlap candidate.
type RelatedPlanHit struct {
	PlanID  string   `json:"plan_id"`
	Name    string   `json:"name"`
	Status  string   `json:"status"`
	Score   int      `json:"score"`
	Reasons []string `json:"reasons"`
}

// RelatedPlansResult is the GET /plans/{id}/related response.
type RelatedPlansResult struct {
	Heuristic  bool             `json:"heuristic"`
	Disclaimer string           `json:"disclaimer"`
	AnchorID   string           `json:"anchor_id"`
	Hits       []RelatedPlanHit `json:"hits"`
}

// PlanView is the Plan CRUD API object (canonical ScrivaDB definition + execution state).
type PlanView struct {
	// Warnings are non-blocking preflight notes; set only on the run-start response.
	Warnings       []string          `json:"warnings,omitempty"`
	ID             string            `json:"id"`
	ProjectID      string            `json:"project_id"`
	Name           string            `json:"name"`
	Goal           string            `json:"goal"`
	FilePath       string            `json:"file_path,omitempty"`
	Status         string            `json:"status"`
	Revision       int64             `json:"revision"`
	ContentHash    string            `json:"content_hash,omitempty"`
	ExecutionMode  string            `json:"execution_mode,omitempty"`
	ExecutorID     string            `json:"executor_id,omitempty"`
	ExportStatus   string            `json:"export_status,omitempty"`
	Constraints    []string          `json:"constraints"`
	DoneWhen       []string          `json:"done_when"`
	Tasks          []PlanTaskSpec    `json:"tasks"`
	TaskProgress   map[string]string `json:"task_progress"`
	TaskSummary    *PlanTaskSummary  `json:"task_summary,omitempty"`
	PlanBranches   []string          `json:"plan_branches,omitempty"`
	AutopilotRunID string            `json:"autopilot_run_id,omitempty"`
	PipelineID     string            `json:"pipeline_id,omitempty"`
	OrchestratorID string            `json:"orchestrator_id,omitempty"`
	RepoExport     *PlanRepoExport   `json:"repo_export,omitempty"`
	Executor       *PlanExecutor     `json:"executor,omitempty"`
	// Ending record (plan-finish-flow §6): decoded here so `plan show` and the
	// MCP tools no longer drop them on re-encode.
	Outcome                          *planstore.PlanOutcome           `json:"outcome,omitempty"`
	CleanupEvidence                  *planstore.CleanupEvidence       `json:"cleanup_evidence,omitempty"`
	ExecutionSummary                 *planstore.ExecutionSummary      `json:"execution_summary,omitempty"`
	ExecutionHistory                 []planstore.PlanExecution        `json:"execution_history,omitempty"`
	TaskOutcomes                     map[string]planstore.TaskOutcome `json:"task_outcomes,omitempty"`
	BranchSummaries                  []planstore.BranchSummary        `json:"branch_summaries,omitempty"`
	IntegrationBranchLeftover        bool                             `json:"integration_branch_leftover,omitempty"`
	IntegrationBranchLeftoverCommits int                              `json:"integration_branch_leftover_commits,omitempty"`
	CreatedAt                        time.Time                        `json:"created_at"`
	UpdatedAt                        time.Time                        `json:"updated_at"`
	StartedAt                        time.Time                        `json:"started_at,omitempty"`
	CompletedAt                      time.Time                        `json:"completed_at,omitempty"`
	ArchivedAt                       time.Time                        `json:"archived_at,omitempty"`
	ArchivedFrom                     string                           `json:"archived_from,omitempty"`
	ArchiveReport                    *PlanArchiveReport               `json:"archive_report,omitempty"`
}

// PlanArchiveReport is what archiving an in-progress plan tore down and kept
// (present only on the archive response).
type PlanArchiveReport struct {
	RemovedAgents   []string `json:"removed_agents,omitempty"`
	RemovedBranches []string `json:"removed_branches,omitempty"`
	RemovedExecutor string   `json:"removed_executor,omitempty"`
	KeptBranches    []struct {
		Branch  string `json:"branch"`
		Commits int    `json:"commits"`
	} `json:"kept_branches,omitempty"`
	Errors []string `json:"errors,omitempty"`
}

// PlanExecutor is the live executor block on GET /plans/{id} (in_progress plans only).
type PlanExecutor struct {
	Kind              string               `json:"kind"`
	ID                string               `json:"id"`
	State             string               `json:"state"`
	Backoff           *PlanExecutorBackoff `json:"backoff,omitempty"`
	IntegrationBranch string               `json:"integration_branch,omitempty"`
	ManagerAgentID    string               `json:"manager_agent_id,omitempty"`
	RestartCount      int                  `json:"restart_count,omitempty"`
	LastRestartReason string               `json:"last_restart_reason,omitempty"`
	LastRestartAt     string               `json:"last_restart_at,omitempty"`
	LastProgressAt    string               `json:"last_progress_at,omitempty"`
	Watchdog          string               `json:"watchdog,omitempty"`
	Tasks             []PlanExecutorTask   `json:"tasks,omitempty"`
}

// PlanExecutorBackoff is the guardian backoff detail, present while degraded.
type PlanExecutorBackoff struct {
	Stage       int    `json:"stage"`
	NextRetryAt string `json:"next_retry_at,omitempty"`
	LastError   string `json:"last_error,omitempty"`
}

// PlanExecutorTask is one task's ledger/job state with its worker and PR.
type PlanExecutorTask struct {
	ID            string `json:"id"`
	State         string `json:"state"`
	WorkerAgentID string `json:"worker_agent_id,omitempty"`
	Branch        string `json:"branch,omitempty"`
	PR            int    `json:"pr,omitempty"`
}

// PlansCreateRequest is the POST /plans body.
type PlansCreateRequest struct {
	ProjectID   string         `json:"project_id"`
	Name        string         `json:"name"`
	Goal        string         `json:"goal"`
	Tasks       []PlanTaskSpec `json:"tasks"`
	Constraints []string       `json:"constraints,omitempty"`
	DoneWhen    []string       `json:"done_when,omitempty"`
}

// PlansUpdateRequest is the PATCH /plans/{id} body. Empty/omitted fields are left unchanged.
type PlansUpdateRequest struct {
	Name             string         `json:"name,omitempty"`
	Goal             string         `json:"goal,omitempty"`
	Tasks            []PlanTaskSpec `json:"tasks,omitempty"`
	Constraints      []string       `json:"constraints,omitempty"`
	DoneWhen         []string       `json:"done_when,omitempty"`
	ExpectedRevision int64          `json:"expected_revision,omitempty"`
}

// PlansTaskAddRequest is the POST /plans/{id}/tasks body.
type PlansTaskAddRequest struct {
	ID               string   `json:"id"`
	Prompt           string   `json:"prompt"`
	After            []string `json:"after,omitempty"`
	ExpectedRevision int64    `json:"expected_revision,omitempty"`
}

// PlansTaskUpdateRequest is the PATCH /plans/{id}/tasks/{task_id}/definition body.
// Nil pointer fields are left unchanged by the daemon.
type PlansTaskUpdateRequest struct {
	Prompt           *string   `json:"prompt,omitempty"`
	After            *[]string `json:"after,omitempty"`
	ExpectedRevision int64     `json:"expected_revision,omitempty"`
}

// PlansList returns plans for a project from GET /api/v1/plans.
func (c *Client) PlansList(ctx context.Context, projectID, status string) ([]PlanView, error) {
	q := url.Values{}
	q.Set("project_id", projectID)
	if status != "" {
		q.Set("status", status)
	}
	var out []PlanView
	if err := c.do(ctx, http.MethodGet, "/plans?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = []PlanView{}
	}
	return out, nil
}

// PlansGet returns one plan from GET /api/v1/plans/{plan_id}.
func (c *Client) PlansGet(ctx context.Context, planID string) (*PlanView, error) {
	var p PlanView
	if err := c.do(ctx, http.MethodGet, "/plans/"+url.PathEscape(planID), nil, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// PlansRelated returns heuristic overlap candidates for a plan.
func (c *Client) PlansRelated(ctx context.Context, planID string, limit int) (*RelatedPlansResult, error) {
	path := "/plans/" + url.PathEscape(planID) + "/related"
	if limit > 0 {
		q := url.Values{}
		q.Set("limit", fmt.Sprintf("%d", limit))
		path += "?" + q.Encode()
	}
	var out RelatedPlansResult
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	if out.Hits == nil {
		out.Hits = []RelatedPlanHit{}
	}
	return &out, nil
}

// PlansCreate inserts a canonical ScrivaDB Plan via POST /api/v1/plans.
func (c *Client) PlansCreate(ctx context.Context, req PlansCreateRequest) (*PlanView, error) {
	var p PlanView
	if err := c.do(ctx, http.MethodPost, "/plans", req, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// PlansUpdate patches a pending plan via PATCH /api/v1/plans/{plan_id}.
func (c *Client) PlansUpdate(ctx context.Context, planID string, req PlansUpdateRequest) (*PlanView, error) {
	var p PlanView
	if err := c.do(ctx, http.MethodPatch, "/plans/"+url.PathEscape(planID), req, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// PlansRun transitions pending → in_progress and starts execution.
func (c *Client) PlansRun(ctx context.Context, planID, executionMode string) (*PlanView, error) {
	var p PlanView
	body := map[string]string{"execution_mode": executionMode}
	if err := c.do(ctx, http.MethodPost, "/plans/"+url.PathEscape(planID)+"/run", body, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// PlansControl pauses, resumes, or stops an in-progress plan's active executor.
func (c *Client) PlansControl(ctx context.Context, planID, action string) (*PlanView, error) {
	var p PlanView
	path := "/plans/" + url.PathEscape(planID) + "/" + url.PathEscape(action)
	if err := c.do(ctx, http.MethodPost, path, nil, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// RestartPlanRequest is the body of POST /plans/{id}/restart.
type RestartPlanRequest struct {
	Force   bool   `json:"force,omitempty"`
	Backend string `json:"backend,omitempty"`
}

// PlansRestart restarts an in_progress plan's executor with a fresh agent set.
// Uses longTimeout — it terminates agents, removes worktrees and respawns.
func (c *Client) PlansRestart(ctx context.Context, planID string, req RestartPlanRequest) (*PlanView, error) {
	var p PlanView
	if err := c.doT(ctx, longTimeout, http.MethodPost, "/plans/"+url.PathEscape(planID)+"/restart", req, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// PlansUpdateTaskStatus merges taskID→status into TaskProgress.
func (c *Client) PlansUpdateTaskStatus(ctx context.Context, planID, taskID, status string) (*PlanView, error) {
	var p PlanView
	path := "/plans/" + url.PathEscape(planID) + "/tasks/" + url.PathEscape(taskID) + "/status"
	if err := c.do(ctx, http.MethodPost, path, map[string]string{"status": status}, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// PlansTaskAdd appends a task to a pending plan via POST /api/v1/plans/{plan_id}/tasks.
func (c *Client) PlansTaskAdd(ctx context.Context, planID string, req PlansTaskAddRequest) (*PlanView, error) {
	var p PlanView
	path := "/plans/" + url.PathEscape(planID) + "/tasks"
	if err := c.do(ctx, http.MethodPost, path, req, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// PlansTaskUpdate patches one task's definition via
// PATCH /api/v1/plans/{plan_id}/tasks/{task_id}/definition.
func (c *Client) PlansTaskUpdate(ctx context.Context, planID, taskID string, req PlansTaskUpdateRequest) (*PlanView, error) {
	var p PlanView
	path := "/plans/" + url.PathEscape(planID) + "/tasks/" + url.PathEscape(taskID) + "/definition"
	if err := c.do(ctx, http.MethodPatch, path, req, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// PlansTaskDelete removes a task via DELETE /api/v1/plans/{plan_id}/tasks/{task_id}.
// When expectedRevision is non-zero it is sent as the expected_revision query param.
func (c *Client) PlansTaskDelete(ctx context.Context, planID, taskID string, expectedRevision int64) (*PlanView, error) {
	var p PlanView
	path := "/plans/" + url.PathEscape(planID) + "/tasks/" + url.PathEscape(taskID)
	if expectedRevision != 0 {
		q := url.Values{}
		q.Set("expected_revision", fmt.Sprintf("%d", expectedRevision))
		path += "?" + q.Encode()
	}
	if err := c.do(ctx, http.MethodDelete, path, nil, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// ParsePlanYAML parses a plan definition YAML document into a PlansUpdateRequest.
// Lifecycle/status/execution fields are ignored. Missing tasks is allowed (partial
// update); when tasks are present the DAG is validated before return.
func ParsePlanYAML(data []byte) (PlansUpdateRequest, error) {
	var doc struct {
		Name        string   `yaml:"name"`
		Goal        string   `yaml:"goal"`
		Constraints []string `yaml:"constraints"`
		DoneWhen    []string `yaml:"done_when"`
		Tasks       []struct {
			ID     string   `yaml:"id"`
			Prompt string   `yaml:"prompt"`
			After  []string `yaml:"after"`
		} `yaml:"tasks"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return PlansUpdateRequest{}, fmt.Errorf("parse plan yaml: %w", err)
	}

	out := PlansUpdateRequest{}
	if name := strings.TrimSpace(doc.Name); name != "" {
		out.Name = name
	}
	if goal := strings.TrimSpace(doc.Goal); goal != "" {
		out.Goal = goal
	}
	if len(doc.Constraints) > 0 {
		out.Constraints = append([]string(nil), doc.Constraints...)
	}
	if len(doc.DoneWhen) > 0 {
		out.DoneWhen = append([]string(nil), doc.DoneWhen...)
	}
	if doc.Tasks != nil {
		tasks := make([]PlanTaskSpec, 0, len(doc.Tasks))
		for _, t := range doc.Tasks {
			tasks = append(tasks, PlanTaskSpec{
				ID:     strings.TrimSpace(t.ID),
				Prompt: strings.TrimSpace(t.Prompt),
				After:  append([]string(nil), t.After...),
			})
		}
		if len(tasks) > 0 {
			specs := make([]planstore.TaskSpec, len(tasks))
			for i, t := range tasks {
				specs[i] = planstore.TaskSpec{ID: t.ID, Prompt: t.Prompt, After: t.After}
			}
			if err := planstore.ValidateTaskDAG(specs); err != nil {
				return PlansUpdateRequest{}, err
			}
		}
		out.Tasks = tasks
	}
	return out, nil
}

// PlansComplete transitions in_progress → completed (422 if tasks/branches block).
// Uses longTimeout — completion tears down the plan's executor, agents and
// worktrees and shells gh once per plan branch. abandonUnmerged opts into
// completing despite an unmerged integration branch (plan-finish-flow §5).
func (c *Client) PlansComplete(ctx context.Context, planID string, abandonUnmerged ...bool) (*PlanView, error) {
	var body any
	if len(abandonUnmerged) > 0 && abandonUnmerged[0] {
		body = map[string]bool{"abandon_unmerged": true}
	}
	var p PlanView
	if err := c.doT(ctx, longTimeout, http.MethodPost, "/plans/"+url.PathEscape(planID)+"/complete", body, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// PlansArchive transitions a plan to archived.
func (c *Client) PlansArchive(ctx context.Context, planID string) (*PlanView, error) {
	var p PlanView
	if err := c.do(ctx, http.MethodPost, "/plans/"+url.PathEscape(planID)+"/archive", nil, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// PlansUnarchive returns an archived plan to the status it was archived from.
func (c *Client) PlansUnarchive(ctx context.Context, planID string) (*PlanView, error) {
	var p PlanView
	if err := c.do(ctx, http.MethodPost, "/plans/"+url.PathEscape(planID)+"/unarchive", nil, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// PlansDelete permanently removes a plan record via DELETE /plans/{id}.
// The daemon refuses in-progress plans with 409.
func (c *Client) PlansDelete(ctx context.Context, planID string) error {
	return c.do(ctx, http.MethodDelete, "/plans/"+url.PathEscape(planID), nil, nil)
}

// PlansSyncToRepoRequest is the POST /plans/{id}/sync_to_repo body.
type PlansSyncToRepoRequest struct {
	TargetRef      string `json:"target_ref"`
	RepositoryPath string `json:"repository_path,omitempty"`
	Format         string `json:"format,omitempty"` // yaml (default) or json
	OutputPath     string `json:"output_path,omitempty"`
	Repository     string `json:"repository,omitempty"`
}

// PlanSyncToRepoResult is the sync_to_repo response.
type PlanSyncToRepoResult struct {
	PlanID       string `json:"plan_id"`
	Revision     int64  `json:"revision"`
	ContentHash  string `json:"content_hash"`
	Repository   string `json:"repository"`
	TargetRef    string `json:"target_ref"`
	OutputPath   string `json:"output_path"`
	Branch       string `json:"branch,omitempty"`
	CommitSHA    string `json:"commit_sha,omitempty"`
	PRURL        string `json:"pr_url,omitempty"`
	PRCreated    bool   `json:"pr_created,omitempty"`
	Outcome      string `json:"outcome"`
	Reason       string `json:"reason,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
	Reused       bool   `json:"reused"`
	RecordID     string `json:"record_id,omitempty"`
}

// PlansSyncToRepo exports a canonical Plan revision onto a dedicated branch and
// opens or reuses a PR. See docs/specs/2026-09-30-scrivadb-canonical-plans.md.
func (c *Client) PlansSyncToRepo(ctx context.Context, planID string, req PlansSyncToRepoRequest) (*PlanSyncToRepoResult, error) {
	var out PlanSyncToRepoResult
	if err := c.do(ctx, http.MethodPost, "/plans/"+url.PathEscape(planID)+"/sync_to_repo", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PlansExportBackupRequest is the POST /plans/export_backup body.
type PlansExportBackupRequest struct {
	PlanIDs   []string `json:"plan_ids,omitempty"`
	ProjectID string   `json:"project_id,omitempty"`
	All       bool     `json:"all,omitempty"`
}

// PlansRestoreBackupRequest is the POST /plans/restore_backup body.
type PlansRestoreBackupRequest struct {
	Bundle     planbackup.Bundle         `json:"bundle"`
	DryRun     bool                      `json:"dry_run,omitempty"`
	OnConflict planbackup.ConflictPolicy `json:"on_conflict,omitempty"`
}

// PlansExportBackup builds a portable Plan backup bundle from ScrivaDB.
func (c *Client) PlansExportBackup(ctx context.Context, req PlansExportBackupRequest) (*planbackup.Bundle, error) {
	var out planbackup.Bundle
	if err := c.do(ctx, http.MethodPost, "/plans/export_backup", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PlansRestoreBackup restores Plans from a portable backup bundle.
func (c *Client) PlansRestoreBackup(ctx context.Context, req PlansRestoreBackupRequest) (*planbackup.RestoreResult, error) {
	var out planbackup.RestoreResult
	if err := c.do(ctx, http.MethodPost, "/plans/restore_backup", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
