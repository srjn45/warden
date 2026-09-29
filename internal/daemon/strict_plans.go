package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/daemon/oapi"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/planstore"
)

// planNotConfigured is returned when the plan store is not wired.
func planNotConfigured() error {
	return errStatus(http.StatusServiceUnavailable, "plan store not configured")
}

// resolvePlanRoot returns the filesystem root of a project given its id. When
// the project store is configured and knows the project, its stored Path is
// used; otherwise project_id is assumed to be the path itself (which is the
// convention for local projects: id == path).
func (s *Server) resolvePlanRoot(projectID string) string {
	if s.projects != nil {
		if p, err := s.projects.Get(projectID); err == nil && p.Path != "" {
			return p.Path
		}
	}
	return projectID
}

// ListPlans implements GET /api/v1/projects/{project_id}/plans.
// Returns a flat list of plans for the project, optionally filtered by status.
func (s *Server) ListPlans(ctx context.Context, req oapi.ListPlansRequestObject) (oapi.ListPlansResponseObject, error) {
	if s.plans == nil {
		return nil, planNotConfigured()
	}

	var plans []*planstore.Plan
	var err error

	if req.Params.Status != "" {
		status := planstore.PlanStatus(req.Params.Status)
		plans, err = s.plans.ListByProjectAndStatus(ctx, req.ProjectId, status)
	} else {
		plans, err = s.plans.ListByProject(ctx, req.ProjectId)
	}
	if err != nil {
		return nil, errStatus(http.StatusInternalServerError, "list plans: "+err.Error())
	}
	if plans == nil {
		plans = []*planstore.Plan{}
	}

	out := make([]planstore.Plan, 0, len(plans))
	for _, p := range plans {
		out = append(out, *p)
	}
	return oapi.ListPlans200JSONResponse{Plans: out}, nil
}

// CreatePlan implements POST /api/v1/projects/{project_id}/plans.
// Creates a new plan record with status pending.
func (s *Server) CreatePlan(ctx context.Context, req oapi.CreatePlanRequestObject) (oapi.CreatePlanResponseObject, error) {
	if s.plans == nil {
		return nil, planNotConfigured()
	}
	if req.Body == nil {
		return oapi.CreatePlan400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{Error: "body is required"}}, nil
	}
	name := strings.TrimSpace(req.Body.Name)
	if name == "" {
		return oapi.CreatePlan400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{Error: "name is required"}}, nil
	}
	filePath := strings.TrimSpace(req.Body.FilePath)
	if filePath == "" {
		return oapi.CreatePlan400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{Error: "file_path is required"}}, nil
	}

	id := planstore.PlanID(req.ProjectId, name)
	p := &planstore.Plan{
		ID:        id,
		ProjectID: req.ProjectId,
		Name:      name,
		FilePath:  filePath,
		Status:    planstore.PlanStatusPending,
	}
	if err := s.plans.Create(ctx, p); err != nil {
		if errors.Is(err, planstore.ErrExists) {
			return oapi.CreatePlan400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{Error: "plan already exists: " + id}}, nil
		}
		return nil, errStatus(http.StatusInternalServerError, "create plan: "+err.Error())
	}
	s.addPlanMembership(p.ID, req.ProjectId)
	got, err := s.plans.Get(ctx, id)
	if err != nil {
		return nil, errStatus(http.StatusInternalServerError, "fetch created plan: "+err.Error())
	}
	return oapi.CreatePlan200JSONResponse(*got), nil
}

// ScanPlans implements POST /api/v1/projects/{project_id}/plans/scan.
// Walks the project's plans/ directory and upserts discovered plans.
// When migrate_flat is true, flat plans/*.yaml files are moved into
// plans/pending/ with git mv and committed.
func (s *Server) ScanPlans(ctx context.Context, req oapi.ScanPlansRequestObject) (oapi.ScanPlansResponseObject, error) {
	if s.plans == nil {
		return nil, planNotConfigured()
	}

	root := s.resolvePlanRoot(req.ProjectId)
	if root == "" {
		return oapi.ScanPlans404JSONResponse{NotFoundJSONResponse: oapi.NotFoundJSONResponse{Error: "project not found"}}, nil
	}

	var migrateFlat, assess bool
	if req.Body != nil {
		migrateFlat = req.Body.MigrateFlat
		assess = req.Body.Assess
	}

	if migrateFlat {
		if err := migrateFlatPlans(ctx, root); err != nil {
			return nil, errStatus(http.StatusInternalServerError, "migrate flat plans: "+err.Error())
		}
	}

	n, err := planstore.ScanProject(ctx, s.plans, req.ProjectId, root)
	if err != nil {
		return nil, errStatus(http.StatusInternalServerError, "scan plans: "+err.Error())
	}
	// Best-effort: membership must never fail the scan itself.
	if plans, listErr := s.plans.ListByProject(ctx, req.ProjectId); listErr != nil {
		slog.Warn("daemon: plan membership: list after scan failed", "project", req.ProjectId, "err", listErr)
	} else {
		for _, p := range plans {
			s.addPlanMembership(p.ID, req.ProjectId)
		}
	}

	_ = assess // Phase 4 stub — assessment not yet implemented

	return oapi.ScanPlans200JSONResponse{Upserted: n}, nil
}

// GetPlan implements GET /api/v1/projects/{project_id}/plans/{plan_id}.
func (s *Server) GetPlan(ctx context.Context, req oapi.GetPlanRequestObject) (oapi.GetPlanResponseObject, error) {
	if s.plans == nil {
		return nil, planNotConfigured()
	}
	p, err := s.plans.Get(ctx, req.PlanId)
	if err != nil {
		if errors.Is(err, planstore.ErrNotFound) {
			return oapi.GetPlan404JSONResponse{NotFoundJSONResponse: oapi.NotFoundJSONResponse{Error: "plan not found"}}, nil
		}
		return nil, errStatus(http.StatusInternalServerError, "get plan: "+err.Error())
	}
	return oapi.GetPlan200JSONResponse(*p), nil
}

// UpdatePlan implements PATCH /api/v1/projects/{project_id}/plans/{plan_id}.
// Allows updating status, execution_mode, and task_progress only.
// Hub-sync fields (synced_at, remote_id) are never writable.
func (s *Server) UpdatePlan(ctx context.Context, req oapi.UpdatePlanRequestObject) (oapi.UpdatePlanResponseObject, error) {
	if s.plans == nil {
		return nil, planNotConfigured()
	}
	if req.Body == nil {
		return oapi.UpdatePlan400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{Error: "body is required"}}, nil
	}

	var updateErr error
	err := s.plans.Update(ctx, req.PlanId, func(p *planstore.Plan) error {
		if req.Body.Status != "" {
			st := planstore.PlanStatus(req.Body.Status)
			if !st.Valid() {
				updateErr = fmt.Errorf("invalid status: %s", st)
				return updateErr
			}
			p.Status = st
		}
		if req.Body.ExecutionMode != "" {
			p.ExecutionMode = planstore.PlanExecutionMode(req.Body.ExecutionMode)
		}
		if req.Body.TaskProgress != nil {
			p.TaskProgress = req.Body.TaskProgress
		}
		// StartedAt / CompletedAt bookkeeping on status transitions.
		if req.Body.Status != "" {
			switch p.Status {
			case planstore.PlanStatusInProgress:
				if p.StartedAt == nil {
					now := time.Now().UTC()
					p.StartedAt = &now
				}
			case planstore.PlanStatusCompleted:
				if p.CompletedAt == nil {
					now := time.Now().UTC()
					p.CompletedAt = &now
				}
			}
		}
		return nil
	})
	if updateErr != nil {
		return oapi.UpdatePlan400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{Error: updateErr.Error()}}, nil
	}
	if err != nil {
		if errors.Is(err, planstore.ErrNotFound) {
			return oapi.UpdatePlan404JSONResponse{NotFoundJSONResponse: oapi.NotFoundJSONResponse{Error: "plan not found"}}, nil
		}
		return nil, errStatus(http.StatusInternalServerError, "update plan: "+err.Error())
	}

	p, err := s.plans.Get(ctx, req.PlanId)
	if err != nil {
		return nil, errStatus(http.StatusInternalServerError, "fetch updated plan: "+err.Error())
	}
	return oapi.UpdatePlan200JSONResponse(*p), nil
}

// DeletePlan implements DELETE /api/v1/projects/{project_id}/plans/{plan_id}.
// Removes the DB record only — never touches the YAML file.
func (s *Server) DeletePlan(ctx context.Context, req oapi.DeletePlanRequestObject) (oapi.DeletePlanResponseObject, error) {
	if s.plans == nil {
		return nil, planNotConfigured()
	}
	if err := s.plans.Delete(ctx, req.PlanId); err != nil {
		if errors.Is(err, planstore.ErrNotFound) {
			return oapi.DeletePlan404JSONResponse{NotFoundJSONResponse: oapi.NotFoundJSONResponse{Error: "plan not found"}}, nil
		}
		return nil, errStatus(http.StatusInternalServerError, "delete plan: "+err.Error())
	}
	s.removePlanMembership(req.PlanId, req.ProjectId)
	return oapi.DeletePlan200JSONResponse{OKJSONResponse: oapi.OKJSONResponse{Status: "deleted"}}, nil
}

// AssessPlan implements POST /api/v1/projects/{project_id}/plans/{plan_id}/assess.
// Reads the plan YAML, calls the brain consultor, and updates task_progress.
func (s *Server) AssessPlan(ctx context.Context, req oapi.AssessPlanRequestObject) (oapi.AssessPlanResponseObject, error) {
	if s.plans == nil {
		return nil, planNotConfigured()
	}

	p, err := s.plans.Get(ctx, req.PlanId)
	if err != nil {
		if errors.Is(err, planstore.ErrNotFound) {
			return oapi.AssessPlan404JSONResponse{NotFoundJSONResponse: oapi.NotFoundJSONResponse{Error: "plan not found"}}, nil
		}
		return nil, errStatus(http.StatusInternalServerError, "get plan: "+err.Error())
	}

	if s.brainConsultor == nil {
		return nil, errStatus(http.StatusServiceUnavailable, "brain consultor not configured")
	}

	root := s.resolvePlanRoot(req.ProjectId)

	progress, err := assessPlanProgress(ctx, p, root, s.brainConsultor)
	if err != nil {
		return nil, errStatus(http.StatusInternalServerError, "assess plan: "+err.Error())
	}

	if len(progress) > 0 {
		if updateErr := s.plans.Update(ctx, req.PlanId, func(pl *planstore.Plan) error {
			if pl.TaskProgress == nil {
				pl.TaskProgress = make(map[string]string)
			}
			for k, v := range progress {
				pl.TaskProgress[k] = v
			}
			return nil
		}); updateErr != nil {
			return nil, errStatus(http.StatusInternalServerError, "update task progress: "+updateErr.Error())
		}
	}

	updated, err := s.plans.Get(ctx, req.PlanId)
	if err != nil {
		return nil, errStatus(http.StatusInternalServerError, "fetch updated plan: "+err.Error())
	}
	return oapi.AssessPlan200JSONResponse(*updated), nil
}

// RunPlan implements POST /api/v1/projects/{project_id}/plans/{plan_id}/run.
// Wires the four execution modes (D15): autopilot, pipeline,
// orchestrator_worker, and manual. In all modes the YAML file is git-mv'd to
// plans/in_progress/ and a commit is created atomically.
func (s *Server) RunPlan(ctx context.Context, req oapi.RunPlanRequestObject) (oapi.RunPlanResponseObject, error) {
	if s.plans == nil {
		return nil, planNotConfigured()
	}
	if req.Body == nil {
		return oapi.RunPlan400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{Error: "body is required"}}, nil
	}

	p, err := s.plans.Get(ctx, req.PlanId)
	if err != nil {
		if errors.Is(err, planstore.ErrNotFound) {
			return oapi.RunPlan404JSONResponse{NotFoundJSONResponse: oapi.NotFoundJSONResponse{Error: "plan not found"}}, nil
		}
		return nil, errStatus(http.StatusInternalServerError, "get plan: "+err.Error())
	}

	// 409 guard: refuse if the plan is already running or done.
	if p.Status == planstore.PlanStatusInProgress || p.Status == planstore.PlanStatusCompleted {
		return oapi.RunPlan409JSONResponse{Error: fmt.Sprintf("plan is already %s", p.Status)}, nil
	}

	mode := planstore.PlanExecutionMode(req.Body.Mode)
	root := s.resolvePlanRoot(req.ProjectId)
	if root == "" {
		return oapi.RunPlan404JSONResponse{NotFoundJSONResponse: oapi.NotFoundJSONResponse{Error: "project not found"}}, nil
	}

	// git-mv the YAML from its current location to plans/in_progress/ and commit.
	newFilePath, err := gitMvPlanStatus(ctx, root, p.FilePath, planstore.PlanStatusInProgress)
	if err != nil {
		return nil, errStatus(http.StatusInternalServerError, "git mv plan to in_progress: "+err.Error())
	}

	// Create the execution entity for this mode.
	var (
		autopilotRunID string
		pipelineID     string
		orchestratorID string
	)

	switch mode {
	case planstore.PlanModeAutopilot:
		if s.autopilot == nil {
			return nil, errStatus(http.StatusServiceUnavailable, "autopilot not configured")
		}
		absPath := filepath.Join(root, newFilePath)
		rs, regErr := s.autopilot.Register(ctx, autopilot.RegisterRequest{
			Name:      p.Name,
			Repo:      root,
			PlanFile:  absPath,
			PlanID:    req.PlanId,
			ProjectID: req.ProjectId,
		})
		if regErr != nil {
			return nil, errStatus(http.StatusInternalServerError, "register autopilot run: "+regErr.Error())
		}
		autopilotRunID = rs.RunID
		s.addAutopilotMembership(autopilotRunID, req.ProjectId)
		s.addPlanMembership(req.PlanId, req.ProjectId)

	case planstore.PlanModePipeline:
		if s.exec == nil {
			return nil, errStatus(http.StatusServiceUnavailable, "pipeline executor not configured")
		}
		pl, buildErr := buildPlanPipeline(p, root)
		if buildErr != nil {
			return nil, errStatus(http.StatusInternalServerError, "build pipeline: "+buildErr.Error())
		}
		pl.PlanID = req.PlanId
		if err := s.exec.pstore.Create(pl); err != nil {
			if errors.Is(err, pipeline.ErrExists) {
				return oapi.RunPlan409JSONResponse{Error: "pipeline for this plan already exists"}, nil
			}
			return nil, errStatus(http.StatusInternalServerError, "create pipeline: "+err.Error())
		}
		s.addPipelineMembership(pl)
		s.addPlanMembership(req.PlanId, req.ProjectId)
		// Start the pipeline immediately.
		_ = s.exec.pstore.Update(pl.ID, func(up *pipeline.Pipeline) { up.Status = pipeline.StatusRunning })
		_ = s.exec.Reconcile(context.Background(), pl.ID)
		pipelineID = pl.ID

	case planstore.PlanModeOrchestratorWorker:
		if s.life == nil {
			return nil, errStatus(http.StatusServiceUnavailable, "lifecycle not configured")
		}
		planContent, readErr := os.ReadFile(filepath.Join(root, newFilePath))
		if readErr != nil {
			return nil, errStatus(http.StatusInternalServerError, "read plan file: "+readErr.Error())
		}
		prompt := fmt.Sprintf("You are an orchestrator executing the following plan.\n\n"+
			"Plan file: %s\n\n%s\n\n"+
			"Execute the plan tasks in order. Each worker you spawn must present its output "+
			"for human approval before you proceed to the next task.",
			newFilePath, string(planContent))
		sr := SpawnRequest{
			Repo:      root,
			Role:      "orchestrator",
			ProjectID: req.ProjectId,
			PlanID:    req.PlanId,
			Prompt:    prompt,
		}
		sess, spawnErr := s.life.Spawn(ctx, sr)
		if spawnErr != nil {
			return nil, errStatus(http.StatusInternalServerError, "spawn orchestrator: "+spawnErr.Error())
		}
		orchestratorID = sess.ID
		s.addPlanMembership(req.PlanId, req.ProjectId)

	case planstore.PlanModeManual:
		// No execution entity — git-mv is the only action.
		s.addPlanMembership(req.PlanId, req.ProjectId)

	default:
		return oapi.RunPlan400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{
			Error: "unknown execution mode: " + string(mode),
		}}, nil
	}

	// Update the DB record atomically.
	now := time.Now().UTC()
	updateErr := s.plans.Update(ctx, req.PlanId, func(pl *planstore.Plan) error {
		pl.FilePath = newFilePath
		pl.Status = planstore.PlanStatusInProgress
		pl.ExecutionMode = mode
		if pl.StartedAt == nil {
			pl.StartedAt = &now
		}
		if autopilotRunID != "" {
			pl.AutopilotRunID = autopilotRunID
		}
		if pipelineID != "" {
			pl.PipelineID = pipelineID
		}
		if orchestratorID != "" {
			pl.OrchestratorID = orchestratorID
		}
		return nil
	})
	if updateErr != nil {
		return nil, errStatus(http.StatusInternalServerError, "update plan record: "+updateErr.Error())
	}

	updated, err := s.plans.Get(ctx, req.PlanId)
	if err != nil {
		return nil, errStatus(http.StatusInternalServerError, "fetch updated plan: "+err.Error())
	}
	return oapi.RunPlan200JSONResponse(*updated), nil
}

// buildPlanPipeline constructs a pipeline.Pipeline with one job per plan task.
// The plan YAML is read from root/plan.FilePath. If the YAML cannot be read or
// has no tasks, an empty single-job pipeline is returned.
func buildPlanPipeline(p *planstore.Plan, root string) (*pipeline.Pipeline, error) {
	var jobs []pipeline.Job
	planPath := filepath.Join(root, p.FilePath)
	if ap, err := autopilot.LoadPlan(planPath); err == nil && len(ap.Tasks) > 0 {
		for _, t := range ap.Tasks {
			jobs = append(jobs, pipeline.Job{
				ID:        t.ID,
				Prompt:    t.Prompt,
				DependsOn: t.After,
				Worktree:  "fresh",
				Type:      "development",
			})
		}
	} else {
		// Fall back to a single job with the plan name as the prompt.
		jobs = []pipeline.Job{{
			ID:       "run",
			Prompt:   "Execute the plan: " + p.Name,
			Worktree: "fresh",
			Type:     "development",
		}}
	}
	pl := &pipeline.Pipeline{
		ID:        p.ID,
		Name:      p.Name,
		Repo:      root,
		ProjectID: p.ProjectID,
		Status:    pipeline.StatusPending,
		Jobs:      jobs,
	}
	if err := pipeline.Validate(pl); err != nil {
		return nil, err
	}
	return pl, nil
}

// gitMvPlanStatus moves a plan YAML from its current path to the subdirectory
// that encodes targetStatus (e.g. "in_progress", "completed"). It performs the
// git mv and creates a commit, then returns the new relative file path.
func gitMvPlanStatus(ctx context.Context, root, currentFilePath string, targetStatus planstore.PlanStatus) (string, error) {
	filename := filepath.Base(currentFilePath)
	newFilePath := filepath.Join("plans", string(targetStatus), filename)
	if currentFilePath == newFilePath {
		return newFilePath, nil
	}

	destDir := filepath.Join(root, "plans", string(targetStatus))
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", err
	}

	out, err := exec.CommandContext(ctx, "git", "-C", root, "mv", currentFilePath, newFilePath).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git mv %s → %s: %w (%s)", currentFilePath, newFilePath, err, strings.TrimSpace(string(out)))
	}

	args := []string{"-C", root}
	if !gitIdentityConfigured(ctx, root) {
		args = append(args, "-c", "user.name=warden", "-c", "user.email=warden@localhost")
	}
	msg := fmt.Sprintf("chore(plans): move %s to %s", filename, targetStatus)
	args = append(args, "commit", "-m", msg)
	if out, err := exec.CommandContext(ctx, "git", args...).CombinedOutput(); err != nil {
		return "", fmt.Errorf("git commit: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return newFilePath, nil
}

// migrateFlatPlans moves all flat plans/*.yaml files into plans/pending/ using
// git mv. If there are files to move it creates a commit. This is idempotent:
// if no flat files exist, it is a no-op.
func migrateFlatPlans(ctx context.Context, root string) error {
	plansDir := filepath.Join(root, "plans")
	entries, err := os.ReadDir(plansDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	pendingDir := filepath.Join(plansDir, "pending")
	if err := os.MkdirAll(pendingDir, 0o755); err != nil {
		return err
	}

	var moved []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		ext := strings.ToLower(filepath.Ext(name))
		if ext != ".yaml" && ext != ".yml" {
			continue
		}
		src := filepath.Join("plans", name)
		dst := filepath.Join("plans", "pending", name)
		out, err := exec.CommandContext(ctx, "git", "-C", root, "mv", src, dst).CombinedOutput()
		if err != nil {
			return fmt.Errorf("git mv %s → %s: %w (%s)", src, dst, err, strings.TrimSpace(string(out)))
		}
		moved = append(moved, name)
	}

	if len(moved) == 0 {
		return nil
	}

	args := []string{"-C", root}
	if !gitIdentityConfigured(ctx, root) {
		args = append(args, "-c", "user.name=warden", "-c", "user.email=warden@localhost")
	}
	args = append(args, "commit", "-m", "chore(plans): migrate flat plans into plans/pending/")
	out, err := exec.CommandContext(ctx, "git", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("git commit: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}
