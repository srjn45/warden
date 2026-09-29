package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

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

// planSvc returns a PlanService bound to this server's store and project-root
// resolver. Nil when the plan store is not wired.
func (s *Server) planSvc() *planstore.PlanService {
	if s.plans == nil {
		return nil
	}
	return planstore.NewPlanService(s.plans, planstore.WithProjectRoot(s.resolvePlanRoot))
}

func oapiTasksToSpec(tasks []oapi.PlanTask) []planstore.TaskSpec {
	out := make([]planstore.TaskSpec, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, planstore.TaskSpec{ID: t.Id, Prompt: t.Prompt, After: t.After})
	}
	return out
}

func planValidationMessage(err error) string {
	var ve *planstore.ValidationError
	if errors.As(err, &ve) {
		return ve.Error()
	}
	return ""
}

func (s *Server) planToOAPI(p *planstore.Plan) oapi.Plan {
	if p == nil {
		return oapi.Plan{}
	}
	taskProgress := make(map[string]oapi.TaskStatus, len(p.TaskProgress))
	for id, status := range p.TaskProgress {
		taskProgress[id] = oapi.TaskStatus(status)
	}
	var startedAt, completedAt time.Time
	if p.StartedAt != nil {
		startedAt = *p.StartedAt
	}
	if p.CompletedAt != nil {
		completedAt = *p.CompletedAt
	}
	out := oapi.Plan{
		AutopilotRunId: p.AutopilotRunID,
		CompletedAt:    completedAt,
		CreatedAt:      p.CreatedAt,
		ExecutionMode:  oapi.PlanExecutionMode(p.ExecutionMode),
		FilePath:       p.FilePath,
		Id:             p.ID,
		Name:           p.Name,
		OrchestratorId: p.OrchestratorID,
		PipelineId:     p.PipelineID,
		PlanBranches:   p.Branches,
		ProjectId:      p.ProjectID,
		StartedAt:      startedAt,
		Status:         oapi.PlanStatus(p.Status),
		TaskProgress:   taskProgress,
		UpdatedAt:      p.UpdatedAt,
	}
	s.hydratePlanDef(p, &out)
	return out
}

// hydratePlanDef fills YAML-backed definition fields (goal, tasks, constraints,
// done_when) onto an API Plan. Missing files are ignored — the DB record is
// still a valid response.
func (s *Server) hydratePlanDef(p *planstore.Plan, out *oapi.Plan) {
	if p == nil || p.FilePath == "" {
		return
	}
	root := s.resolvePlanRoot(p.ProjectID)
	if root == "" {
		return
	}
	data, err := os.ReadFile(filepath.Join(root, p.FilePath))
	if err != nil {
		return
	}
	var doc struct {
		Goal        string   `yaml:"goal"`
		Constraints []string `yaml:"constraints"`
		DoneWhen    []string `yaml:"done_when"`
		Tasks       []struct {
			ID     string   `yaml:"id"`
			Prompt string   `yaml:"prompt"`
			After  []string `yaml:"after"`
		} `yaml:"tasks"`
	}
	if yaml.Unmarshal(data, &doc) != nil {
		return
	}
	out.Goal = doc.Goal
	out.Constraints = doc.Constraints
	out.DoneWhen = doc.DoneWhen
	out.Tasks = make([]oapi.PlanTask, 0, len(doc.Tasks))
	for _, t := range doc.Tasks {
		out.Tasks = append(out.Tasks, oapi.PlanTask{Id: t.ID, Prompt: t.Prompt, After: t.After})
	}
}

// ListProjectPlans preserves the original project-scoped plan-list API.
func (s *Server) ListProjectPlans(ctx context.Context, req oapi.ListProjectPlansRequestObject) (oapi.ListProjectPlansResponseObject, error) {
	if s.plans == nil {
		return nil, planNotConfigured()
	}
	var (
		plans []*planstore.Plan
		err   error
	)
	if req.Params.Status != "" {
		plans, err = s.plans.ListByProjectAndStatus(ctx, req.ProjectId, planstore.PlanStatus(req.Params.Status))
	} else {
		plans, err = s.plans.ListByProject(ctx, req.ProjectId)
	}
	if err != nil {
		return nil, errStatus(http.StatusInternalServerError, "list plans: "+err.Error())
	}
	out := make([]planstore.Plan, 0, len(plans))
	for _, p := range plans {
		out = append(out, *p)
	}
	return oapi.ListProjectPlans200JSONResponse{Plans: out}, nil
}

// CreateProjectPlan preserves the original project-scoped record API.
func (s *Server) CreateProjectPlan(ctx context.Context, req oapi.CreateProjectPlanRequestObject) (oapi.CreateProjectPlanResponseObject, error) {
	if s.plans == nil {
		return nil, planNotConfigured()
	}
	if req.Body == nil {
		return oapi.CreateProjectPlan400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{Error: "body is required"}}, nil
	}
	name, filePath := strings.TrimSpace(req.Body.Name), strings.TrimSpace(req.Body.FilePath)
	if name == "" || filePath == "" {
		return oapi.CreateProjectPlan400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{Error: "name and file_path are required"}}, nil
	}
	p := &planstore.Plan{ID: planstore.PlanID(req.ProjectId, name), ProjectID: req.ProjectId, Name: name, FilePath: filePath, Status: planstore.PlanStatusPending}
	if err := s.plans.Create(ctx, p); err != nil {
		if errors.Is(err, planstore.ErrExists) {
			return oapi.CreateProjectPlan400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{Error: "plan already exists: " + p.ID}}, nil
		}
		return nil, errStatus(http.StatusInternalServerError, "create plan: "+err.Error())
	}
	got, err := s.plans.Get(ctx, p.ID)
	if err != nil {
		return nil, errStatus(http.StatusInternalServerError, "fetch created plan: "+err.Error())
	}
	return oapi.CreateProjectPlan200JSONResponse(*got), nil
}

func (s *Server) ScanProjectPlans(ctx context.Context, req oapi.ScanProjectPlansRequestObject) (oapi.ScanProjectPlansResponseObject, error) {
	if s.plans == nil {
		return nil, planNotConfigured()
	}
	root := s.resolvePlanRoot(req.ProjectId)
	if root == "" {
		return oapi.ScanProjectPlans404JSONResponse{NotFoundJSONResponse: oapi.NotFoundJSONResponse{Error: "project not found"}}, nil
	}
	if req.Body != nil && req.Body.MigrateFlat {
		if err := migrateFlatPlans(ctx, root); err != nil {
			return nil, errStatus(http.StatusInternalServerError, "migrate flat plans: "+err.Error())
		}
	}
	n, err := planstore.ScanProject(ctx, s.plans, req.ProjectId, root)
	if err != nil {
		return nil, errStatus(http.StatusInternalServerError, "scan plans: "+err.Error())
	}
	return oapi.ScanProjectPlans200JSONResponse{Upserted: n}, nil
}

func (s *Server) GetProjectPlan(ctx context.Context, req oapi.GetProjectPlanRequestObject) (oapi.GetProjectPlanResponseObject, error) {
	if s.plans == nil {
		return nil, planNotConfigured()
	}
	p, err := s.plans.Get(ctx, req.PlanId)
	if errors.Is(err, planstore.ErrNotFound) {
		return oapi.GetProjectPlan404JSONResponse{NotFoundJSONResponse: oapi.NotFoundJSONResponse{Error: "plan not found"}}, nil
	}
	if err != nil {
		return nil, errStatus(http.StatusInternalServerError, "get plan: "+err.Error())
	}
	return oapi.GetProjectPlan200JSONResponse(*p), nil
}

func (s *Server) UpdateProjectPlan(ctx context.Context, req oapi.UpdateProjectPlanRequestObject) (oapi.UpdateProjectPlanResponseObject, error) {
	if s.plans == nil {
		return nil, planNotConfigured()
	}
	if req.Body == nil {
		return oapi.UpdateProjectPlan400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{Error: "body is required"}}, nil
	}
	p, err := s.legacyUpdatePlanStatus(ctx, req.PlanId, planstore.PlanStatus(req.Body.Status), planstore.PlanExecutionMode(req.Body.ExecutionMode), req.Body.TaskProgress)
	if errors.Is(err, planstore.ErrNotFound) {
		return oapi.UpdateProjectPlan404JSONResponse{NotFoundJSONResponse: oapi.NotFoundJSONResponse{Error: "plan not found"}}, nil
	}
	if err != nil {
		return oapi.UpdateProjectPlan400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{Error: err.Error()}}, nil
	}
	return oapi.UpdateProjectPlan200JSONResponse(*p), nil
}

func (s *Server) DeleteProjectPlan(ctx context.Context, req oapi.DeleteProjectPlanRequestObject) (oapi.DeleteProjectPlanResponseObject, error) {
	if s.plans == nil {
		return nil, planNotConfigured()
	}
	if err := s.plans.Delete(ctx, req.PlanId); errors.Is(err, planstore.ErrNotFound) {
		return oapi.DeleteProjectPlan404JSONResponse{NotFoundJSONResponse: oapi.NotFoundJSONResponse{Error: "plan not found"}}, nil
	} else if err != nil {
		return nil, errStatus(http.StatusInternalServerError, "delete plan: "+err.Error())
	}
	return oapi.DeleteProjectPlan200JSONResponse{OKJSONResponse: oapi.OKJSONResponse{Status: "deleted"}}, nil
}

func (s *Server) AssessProjectPlan(ctx context.Context, req oapi.AssessProjectPlanRequestObject) (oapi.AssessProjectPlanResponseObject, error) {
	if s.plans == nil {
		return nil, planNotConfigured()
	}
	p, err := s.plans.Get(ctx, req.PlanId)
	if errors.Is(err, planstore.ErrNotFound) {
		return oapi.AssessProjectPlan404JSONResponse{NotFoundJSONResponse: oapi.NotFoundJSONResponse{Error: "plan not found"}}, nil
	}
	if err != nil {
		return nil, errStatus(http.StatusInternalServerError, "get plan: "+err.Error())
	}
	if s.brainConsultor == nil {
		return nil, errStatus(http.StatusServiceUnavailable, "brain consultor not configured")
	}
	progress, err := assessPlanProgress(ctx, p, s.resolvePlanRoot(req.ProjectId), s.brainConsultor)
	if err != nil {
		return nil, errStatus(http.StatusInternalServerError, "assess plan: "+err.Error())
	}
	if len(progress) > 0 {
		if err := s.plans.Update(ctx, req.PlanId, func(p *planstore.Plan) error {
			if p.TaskProgress == nil {
				p.TaskProgress = map[string]string{}
			}
			for id, status := range progress {
				p.TaskProgress[id] = status
			}
			return nil
		}); err != nil {
			return nil, errStatus(http.StatusInternalServerError, "update task progress: "+err.Error())
		}
		p, err = s.plans.Get(ctx, req.PlanId)
		if err != nil {
			return nil, errStatus(http.StatusInternalServerError, "fetch updated plan: "+err.Error())
		}
	}
	return oapi.AssessProjectPlan200JSONResponse(*p), nil
}

func (s *Server) RunProjectPlan(ctx context.Context, req oapi.RunProjectPlanRequestObject) (oapi.RunProjectPlanResponseObject, error) {
	if req.Body == nil {
		return oapi.RunProjectPlan400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{Error: "body is required"}}, nil
	}
	response, err := s.RunPlan(ctx, oapi.RunPlanRequestObject{PlanId: req.PlanId, Body: &oapi.RunPlanRequest{ExecutionMode: oapi.RunPlanRequestExecutionMode(req.Body.Mode)}})
	if err != nil {
		return nil, err
	}
	switch response.(type) {
	case oapi.RunPlan200JSONResponse:
		p, err := s.plans.Get(ctx, req.PlanId)
		if err != nil {
			return nil, errStatus(http.StatusInternalServerError, "fetch started plan: "+err.Error())
		}
		return oapi.RunProjectPlan200JSONResponse(*p), nil
	case oapi.RunPlan400JSONResponse:
		return oapi.RunProjectPlan400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{Error: "invalid run request"}}, nil
	case oapi.RunPlan404JSONResponse:
		return oapi.RunProjectPlan404JSONResponse{NotFoundJSONResponse: oapi.NotFoundJSONResponse{Error: "plan not found"}}, nil
	case oapi.RunPlan409JSONResponse:
		return oapi.RunProjectPlan409JSONResponse{Error: "plan cannot transition to in_progress"}, nil
	default:
		return nil, errStatus(http.StatusInternalServerError, "unexpected run plan response")
	}
}

// ListPlans implements GET /api/v1/plans.
func (s *Server) ListPlans(ctx context.Context, req oapi.ListPlansRequestObject) (oapi.ListPlansResponseObject, error) {
	svc := s.planSvc()
	if svc == nil {
		return nil, planNotConfigured()
	}
	if strings.TrimSpace(req.Params.ProjectId) == "" {
		return oapi.ListPlans400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{Error: "project_id is required"}}, nil
	}
	plans, err := svc.List(ctx, req.Params.ProjectId, planstore.PlanStatus(req.Params.Status))
	if err != nil {
		return nil, errStatus(http.StatusInternalServerError, "list plans: "+err.Error())
	}
	out := make([]oapi.Plan, 0, len(plans))
	for _, p := range plans {
		out = append(out, s.planToOAPI(p))
	}
	return oapi.ListPlans200JSONResponse(out), nil
}

// CreatePlan implements POST /api/v1/plans.
func (s *Server) CreatePlan(ctx context.Context, req oapi.CreatePlanRequestObject) (oapi.CreatePlanResponseObject, error) {
	svc := s.planSvc()
	if svc == nil {
		return nil, planNotConfigured()
	}
	if req.Body == nil {
		return oapi.CreatePlan400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{Error: "body is required"}}, nil
	}
	projectID := strings.TrimSpace(req.Body.ProjectId)
	if projectID == "" {
		return oapi.CreatePlan400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{Error: "project_id: project_id is required"}}, nil
	}
	if s.projects != nil {
		if _, err := s.projects.Get(projectID); err != nil {
			return oapi.CreatePlan404JSONResponse{NotFoundJSONResponse: oapi.NotFoundJSONResponse{Error: "project not found"}}, nil
		}
	}
	created, err := svc.Create(ctx, projectID, planstore.CreateRequest{
		Name:        req.Body.Name,
		Goal:        req.Body.Goal,
		Tasks:       oapiTasksToSpec(req.Body.Tasks),
		Constraints: req.Body.Constraints,
		DoneWhen:    req.Body.DoneWhen,
	})
	if err != nil {
		if msg := planValidationMessage(err); msg != "" {
			return oapi.CreatePlan400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{Error: msg}}, nil
		}
		if errors.Is(err, planstore.ErrExists) {
			return oapi.CreatePlan400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{Error: err.Error()}}, nil
		}
		return nil, errStatus(http.StatusInternalServerError, "create plan: "+err.Error())
	}
	return oapi.CreatePlan201JSONResponse(s.planToOAPI(created)), nil
}

// GetPlan implements GET /api/v1/plans/{plan_id}.
func (s *Server) GetPlan(ctx context.Context, req oapi.GetPlanRequestObject) (oapi.GetPlanResponseObject, error) {
	svc := s.planSvc()
	if svc == nil {
		return nil, planNotConfigured()
	}
	p, err := svc.Get(ctx, req.PlanId)
	if err != nil {
		if errors.Is(err, planstore.ErrNotFound) {
			return oapi.GetPlan404JSONResponse{NotFoundJSONResponse: oapi.NotFoundJSONResponse{Error: "plan not found"}}, nil
		}
		return nil, errStatus(http.StatusInternalServerError, "get plan: "+err.Error())
	}
	return oapi.GetPlan200JSONResponse(s.planToOAPI(p)), nil
}

// UpdatePlan implements PATCH /api/v1/plans/{plan_id}.
func (s *Server) UpdatePlan(ctx context.Context, req oapi.UpdatePlanRequestObject) (oapi.UpdatePlanResponseObject, error) {
	svc := s.planSvc()
	if svc == nil {
		return nil, planNotConfigured()
	}
	if req.Body == nil {
		return oapi.UpdatePlan400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{Error: "body is required"}}, nil
	}
	upd := updateRequestFromBody(req.Body)
	p, err := svc.Update(ctx, req.PlanId, upd)
	if err != nil {
		if errors.Is(err, planstore.ErrNotFound) {
			return oapi.UpdatePlan404JSONResponse{NotFoundJSONResponse: oapi.NotFoundJSONResponse{Error: "plan not found"}}, nil
		}
		if errors.Is(err, planstore.ErrNotPending) {
			return oapi.UpdatePlan409JSONResponse{Error: err.Error()}, nil
		}
		if msg := planValidationMessage(err); msg != "" {
			return oapi.UpdatePlan400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{Error: msg}}, nil
		}
		return nil, errStatus(http.StatusInternalServerError, "update plan: "+err.Error())
	}
	return oapi.UpdatePlan200JSONResponse(s.planToOAPI(p)), nil
}

func updateRequestFromBody(body *oapi.UpdatePlanRequest) planstore.UpdateRequest {
	upd := planstore.UpdateRequest{}
	if body == nil {
		return upd
	}
	if name := strings.TrimSpace(body.Name); name != "" {
		upd.Name = &name
	}
	if body.Goal != "" {
		g := body.Goal
		upd.Goal = &g
	}
	if body.Tasks != nil {
		tasks := oapiTasksToSpec(body.Tasks)
		upd.Tasks = &tasks
	}
	if body.Constraints != nil {
		c := body.Constraints
		upd.Constraints = &c
	}
	if body.DoneWhen != nil {
		d := body.DoneWhen
		upd.DoneWhen = &d
	}
	return upd
}

// UpdateTaskStatus implements POST /api/v1/plans/{plan_id}/tasks/{task_id}/status.
func (s *Server) UpdateTaskStatus(ctx context.Context, req oapi.UpdateTaskStatusRequestObject) (oapi.UpdateTaskStatusResponseObject, error) {
	svc := s.planSvc()
	if svc == nil {
		return nil, planNotConfigured()
	}
	if req.Body == nil {
		return oapi.UpdateTaskStatus400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{Error: "body is required"}}, nil
	}
	p, err := svc.UpdateTaskStatus(ctx, req.PlanId, req.TaskId, string(req.Body.Status))
	if err != nil {
		if errors.Is(err, planstore.ErrNotFound) {
			return oapi.UpdateTaskStatus404JSONResponse{NotFoundJSONResponse: oapi.NotFoundJSONResponse{Error: "plan not found"}}, nil
		}
		if errors.Is(err, planstore.ErrInvalidTaskStatus) || planValidationMessage(err) != "" {
			msg := err.Error()
			if v := planValidationMessage(err); v != "" {
				msg = v
			}
			return oapi.UpdateTaskStatus400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{Error: msg}}, nil
		}
		return nil, errStatus(http.StatusInternalServerError, "update task status: "+err.Error())
	}
	return oapi.UpdateTaskStatus200JSONResponse(s.planToOAPI(p)), nil
}

// ArchivePlan implements POST /api/v1/plans/{plan_id}/archive.
func (s *Server) ArchivePlan(ctx context.Context, req oapi.ArchivePlanRequestObject) (oapi.ArchivePlanResponseObject, error) {
	svc := s.planSvc()
	if svc == nil {
		return nil, planNotConfigured()
	}
	p, err := svc.Transition(ctx, req.PlanId, planstore.PlanStatusArchived, planstore.TransitionOptions{})
	if err != nil {
		if errors.Is(err, planstore.ErrNotFound) {
			return oapi.ArchivePlan404JSONResponse{NotFoundJSONResponse: oapi.NotFoundJSONResponse{Error: "plan not found"}}, nil
		}
		if errors.Is(err, planstore.ErrInvalidTransition) {
			return nil, errStatus(http.StatusConflict, err.Error())
		}
		return nil, errStatus(http.StatusInternalServerError, "archive plan: "+err.Error())
	}
	return oapi.ArchivePlan200JSONResponse(s.planToOAPI(p)), nil
}

// CompletePlan implements POST /api/v1/plans/{plan_id}/complete.
func (s *Server) CompletePlan(ctx context.Context, req oapi.CompletePlanRequestObject) (oapi.CompletePlanResponseObject, error) {
	svc := s.planSvc()
	if svc == nil {
		return nil, planNotConfigured()
	}
	p, err := svc.Transition(ctx, req.PlanId, planstore.PlanStatusCompleted, planstore.TransitionOptions{})
	if err != nil {
		if errors.Is(err, planstore.ErrNotFound) {
			return oapi.CompletePlan404JSONResponse{NotFoundJSONResponse: oapi.NotFoundJSONResponse{Error: "plan not found"}}, nil
		}
		if errors.Is(err, planstore.ErrInvalidTransition) {
			return oapi.CompletePlan409JSONResponse{Error: err.Error()}, nil
		}
		var incomplete *planstore.TasksIncompleteError
		if errors.As(err, &incomplete) {
			return oapi.CompletePlan422JSONResponse{Error: incomplete.Error(), IncompleteTasks: incomplete.TaskIDs}, nil
		}
		var unmerged *planstore.BranchesUnmergedError
		if errors.As(err, &unmerged) {
			return oapi.CompletePlan422JSONResponse{Error: unmerged.Error(), UnmergedBranches: unmerged.Branches}, nil
		}
		return nil, errStatus(http.StatusInternalServerError, "complete plan: "+err.Error())
	}
	_ = svc.CleanupWorktrees(ctx, p)
	return oapi.CompletePlan200JSONResponse(s.planToOAPI(p)), nil
}

// legacyUpdatePlanStatus preserves the pre-CRUD route tests until the new
// service layer takes ownership of plan transitions.
func (s *Server) legacyUpdatePlanStatus(ctx context.Context, planID string, status planstore.PlanStatus, mode planstore.PlanExecutionMode, progress map[string]string) (*planstore.Plan, error) {
	if err := s.plans.Update(ctx, planID, func(p *planstore.Plan) error {
		if status != "" {
			if !status.Valid() {
				return fmt.Errorf("invalid status: %s", status)
			}
			p.Status = status
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
		if mode != "" {
			p.ExecutionMode = mode
		}
		if progress != nil {
			p.TaskProgress = progress
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return s.plans.Get(ctx, planID)
}

// RunPlan implements POST /api/v1/plans/{plan_id}/run.
// Transitions pending → in_progress via PlanService, then starts the selected
// execution mode (autopilot / pipeline / orchestrator_worker / manual).
func (s *Server) RunPlan(ctx context.Context, req oapi.RunPlanRequestObject) (oapi.RunPlanResponseObject, error) {
	svc := s.planSvc()
	if svc == nil {
		return nil, planNotConfigured()
	}
	if req.Body == nil {
		return oapi.RunPlan400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{Error: "body is required"}}, nil
	}

	mode := planstore.PlanExecutionMode(req.Body.ExecutionMode)
	switch mode {
	case planstore.PlanModeAutopilot, planstore.PlanModePipeline, planstore.PlanModeOrchestratorWorker, planstore.PlanModeManual:
	default:
		return oapi.RunPlan400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{
			Error: "unknown execution mode: " + string(mode),
		}}, nil
	}

	// Refuse a known-unconfigured backend before moving the YAML.
	switch mode {
	case planstore.PlanModeAutopilot:
		if s.autopilot == nil {
			return nil, errStatus(http.StatusServiceUnavailable, "autopilot not configured")
		}
	case planstore.PlanModePipeline:
		if s.exec == nil {
			return nil, errStatus(http.StatusServiceUnavailable, "pipeline executor not configured")
		}
	case planstore.PlanModeOrchestratorWorker:
		if s.life == nil {
			return nil, errStatus(http.StatusServiceUnavailable, "lifecycle not configured")
		}
	}

	p, err := svc.Transition(ctx, req.PlanId, planstore.PlanStatusInProgress, planstore.TransitionOptions{ExecutionMode: mode})
	if err != nil {
		if errors.Is(err, planstore.ErrNotFound) {
			return oapi.RunPlan404JSONResponse{NotFoundJSONResponse: oapi.NotFoundJSONResponse{Error: "plan not found"}}, nil
		}
		if errors.Is(err, planstore.ErrInvalidTransition) {
			return oapi.RunPlan409JSONResponse{Error: err.Error()}, nil
		}
		if msg := planValidationMessage(err); msg != "" {
			return oapi.RunPlan400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{Error: msg}}, nil
		}
		return nil, errStatus(http.StatusInternalServerError, "run plan: "+err.Error())
	}

	if err := s.startPlanExecution(ctx, p, mode); err != nil {
		if errors.Is(err, pipeline.ErrExists) {
			return oapi.RunPlan409JSONResponse{Error: "pipeline for this plan already exists"}, nil
		}
		return nil, err
	}

	updated, err := svc.Get(ctx, req.PlanId)
	if err != nil {
		return nil, errStatus(http.StatusInternalServerError, "fetch updated plan: "+err.Error())
	}
	return oapi.RunPlan200JSONResponse(s.planToOAPI(updated)), nil
}

// startPlanExecution creates the execution entity for mode and records its id
// on the plan. The YAML has already been moved to plans/in_progress/.
func (s *Server) startPlanExecution(ctx context.Context, p *planstore.Plan, mode planstore.PlanExecutionMode) error {
	root := s.resolvePlanRoot(p.ProjectID)
	if root == "" {
		return errStatus(http.StatusNotFound, "project not found")
	}

	var (
		autopilotRunID string
		pipelineID     string
		orchestratorID string
	)

	switch mode {
	case planstore.PlanModeAutopilot:
		if s.autopilot == nil {
			return errStatus(http.StatusServiceUnavailable, "autopilot not configured")
		}
		rs, regErr := s.autopilot.Register(ctx, autopilot.RegisterRequest{
			Name:     p.Name,
			Repo:     root,
			PlanFile: filepath.Join(root, p.FilePath),
		})
		if regErr != nil {
			return errStatus(http.StatusInternalServerError, "register autopilot run: "+regErr.Error())
		}
		autopilotRunID = rs.RunID

	case planstore.PlanModePipeline:
		if s.exec == nil {
			return errStatus(http.StatusServiceUnavailable, "pipeline executor not configured")
		}
		pl, buildErr := buildPlanPipeline(p, root)
		if buildErr != nil {
			return errStatus(http.StatusInternalServerError, "build pipeline: "+buildErr.Error())
		}
		if err := s.exec.pstore.Create(pl); err != nil {
			if errors.Is(err, pipeline.ErrExists) {
				return err
			}
			return errStatus(http.StatusInternalServerError, "create pipeline: "+err.Error())
		}
		s.addPipelineMembership(pl)
		_ = s.exec.pstore.Update(pl.ID, func(up *pipeline.Pipeline) { up.Status = pipeline.StatusRunning })
		_ = s.exec.Reconcile(context.Background(), pl.ID)
		pipelineID = pl.ID

	case planstore.PlanModeOrchestratorWorker:
		if s.life == nil {
			return errStatus(http.StatusServiceUnavailable, "lifecycle not configured")
		}
		planContent, readErr := os.ReadFile(filepath.Join(root, p.FilePath))
		if readErr != nil {
			return errStatus(http.StatusInternalServerError, "read plan file: "+readErr.Error())
		}
		prompt := fmt.Sprintf("You are an orchestrator executing the following plan.\n\n"+
			"Plan file: %s\n\n%s\n\n"+
			"Execute the plan tasks in order. Each worker you spawn must present its output "+
			"for human approval before you proceed to the next task.",
			p.FilePath, string(planContent))
		sess, spawnErr := s.life.Spawn(ctx, SpawnRequest{
			Repo:      root,
			Role:      "orchestrator",
			ProjectID: p.ProjectID,
			Prompt:    prompt,
		})
		if spawnErr != nil {
			return errStatus(http.StatusInternalServerError, "spawn orchestrator: "+spawnErr.Error())
		}
		orchestratorID = sess.ID

	case planstore.PlanModeManual:
		return nil

	default:
		return errStatus(http.StatusBadRequest, "unknown execution mode: "+string(mode))
	}

	if autopilotRunID == "" && pipelineID == "" && orchestratorID == "" {
		return nil
	}
	if err := s.plans.Update(ctx, p.ID, func(pl *planstore.Plan) error {
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
	}); err != nil {
		return errStatus(http.StatusInternalServerError, "record execution link: "+err.Error())
	}
	return nil
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
