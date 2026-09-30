package client

import (
	"context"
	"net/http"
	"net/url"
	"time"
)

// PlanTaskSpec is one task in a create/update request or a hydrated Plan.
type PlanTaskSpec struct {
	ID     string   `json:"id"`
	Prompt string   `json:"prompt"`
	After  []string `json:"after,omitempty"`
}

// PlanView is the Plan CRUD API object (canonical ScrivaDB definition + execution state).
type PlanView struct {
	ID             string            `json:"id"`
	ProjectID      string            `json:"project_id"`
	Name           string            `json:"name"`
	Goal           string            `json:"goal"`
	FilePath       string            `json:"file_path,omitempty"`
	Status         string            `json:"status"`
	Revision       int64             `json:"revision"`
	ContentHash    string            `json:"content_hash,omitempty"`
	ExecutionMode  string            `json:"execution_mode,omitempty"`
	Constraints    []string          `json:"constraints"`
	DoneWhen       []string          `json:"done_when"`
	Tasks          []PlanTaskSpec    `json:"tasks"`
	TaskProgress   map[string]string `json:"task_progress"`
	PlanBranches   []string          `json:"plan_branches,omitempty"`
	AutopilotRunID string            `json:"autopilot_run_id,omitempty"`
	PipelineID     string            `json:"pipeline_id,omitempty"`
	OrchestratorID string            `json:"orchestrator_id,omitempty"`
	CreatedAt      time.Time         `json:"created_at"`
	UpdatedAt      time.Time         `json:"updated_at"`
	StartedAt      time.Time         `json:"started_at,omitempty"`
	CompletedAt    time.Time         `json:"completed_at,omitempty"`
	ArchivedAt     time.Time         `json:"archived_at,omitempty"`
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

// PlansUpdateTaskStatus merges taskID→status into TaskProgress.
func (c *Client) PlansUpdateTaskStatus(ctx context.Context, planID, taskID, status string) (*PlanView, error) {
	var p PlanView
	path := "/plans/" + url.PathEscape(planID) + "/tasks/" + url.PathEscape(taskID) + "/status"
	if err := c.do(ctx, http.MethodPost, path, map[string]string{"status": status}, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// PlansComplete transitions in_progress → completed (422 if tasks/branches block).
func (c *Client) PlansComplete(ctx context.Context, planID string) (*PlanView, error) {
	var p PlanView
	if err := c.do(ctx, http.MethodPost, "/plans/"+url.PathEscape(planID)+"/complete", nil, &p); err != nil {
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
