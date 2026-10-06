package daemon

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/srjn45/warden/internal/audit"
	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/daemon/oapi"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/planstore"
)

// ControlPlan implements POST /api/v1/plans/{plan_id}/{action}.
// Pause/resume/stop the active executor for an in-progress plan.
func (s *Server) ControlPlan(ctx context.Context, req oapi.ControlPlanRequestObject) (oapi.ControlPlanResponseObject, error) {
	svc := s.planSvc()
	if svc == nil {
		return nil, planNotConfigured()
	}
	action := strings.TrimSpace(req.Action)
	switch action {
	case "pause", "resume", "stop":
	default:
		return oapi.ControlPlan400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{
			Error: "action must be pause, resume, or stop",
		}}, nil
	}

	p, err := svc.Get(ctx, req.PlanId)
	if err != nil {
		if errors.Is(err, planstore.ErrNotFound) {
			return oapi.ControlPlan404JSONResponse{NotFoundJSONResponse: oapi.NotFoundJSONResponse{Error: "plan not found"}}, nil
		}
		return nil, errStatus(http.StatusInternalServerError, "get plan: "+err.Error())
	}
	if p.Status != planstore.PlanStatusInProgress {
		return oapi.ControlPlan409JSONResponse{Error: "plan is not in_progress"}, nil
	}

	switch {
	case p.AutopilotRunID != "" && s.autopilot != nil:
		if err := s.controlPlanAutopilot(ctx, p.AutopilotRunID, action); err != nil {
			return mapPlanControlErr(err)
		}
	case p.PipelineID != "" && s.exec != nil:
		if err := s.controlPlanPipeline(ctx, p.PipelineID, action); err != nil {
			return mapPlanControlErr(err)
		}
	case (p.OrchestratorID != "" || (p.ActiveExecution != nil && p.ActiveExecution.ExecutorID != "")) && s.life != nil:
		executorID := p.OrchestratorID
		if executorID == "" && p.ActiveExecution != nil {
			executorID = p.ActiveExecution.ExecutorID
		}
		if err := s.controlPlanAgent(ctx, executorID, action); err != nil {
			return mapPlanControlErr(err)
		}
	default:
		return oapi.ControlPlan409JSONResponse{Error: "plan has no active executor to control"}, nil
	}

	updated, err := svc.Get(ctx, req.PlanId)
	if err != nil {
		return nil, errStatus(http.StatusInternalServerError, "fetch updated plan: "+err.Error())
	}
	s.recordAuditCtx(ctx, audit.ActionAutopilotOn, req.PlanId, map[string]string{"action": action, "surface": "plan_control"})
	return oapi.ControlPlan200JSONResponse(s.planToOAPI(updated)), nil
}

func (s *Server) controlPlanAutopilot(ctx context.Context, runID, action string) error {
	var err error
	switch action {
	case "pause":
		_, err = s.autopilot.PauseRun(ctx, runID)
	case "resume":
		_, err = s.autopilot.ResumeRun(ctx, runID)
	case "stop":
		_, err = s.autopilot.StopRun(ctx, runID)
	}
	return err
}

func (s *Server) controlPlanPipeline(ctx context.Context, pipelineID, action string) error {
	switch action {
	case "pause":
		return s.exec.Pause(pipelineID)
	case "resume":
		return s.exec.Resume(ctx, pipelineID)
	case "stop":
		return s.cancelPipeline(ctx, pipelineID)
	}
	return nil
}

func (s *Server) controlPlanAgent(ctx context.Context, agentID, action string) error {
	switch action {
	case "pause", "resume":
		return errStatus(http.StatusConflict, "pause/resume is not supported for orchestrator/manual plan executors; use stop")
	case "stop":
		_, err := s.TerminateSession(ctx, oapi.TerminateSessionRequestObject{Id: agentID})
		return err
	}
	return nil
}

func mapPlanControlErr(err error) (oapi.ControlPlanResponseObject, error) {
	if err == nil {
		return nil, nil
	}
	if errors.Is(err, autopilot.ErrRunNotFound) || errors.Is(err, pipeline.ErrNotFound) {
		return oapi.ControlPlan404JSONResponse{NotFoundJSONResponse: oapi.NotFoundJSONResponse{Error: err.Error()}}, nil
	}
	if errors.Is(err, autopilot.ErrRunConflict) || errors.Is(err, ErrNotPausable) || errors.Is(err, ErrNotPaused) {
		return oapi.ControlPlan409JSONResponse{Error: err.Error()}, nil
	}
	var ae apiError
	if errors.As(err, &ae) {
		switch ae.code {
		case http.StatusNotFound:
			return oapi.ControlPlan404JSONResponse{NotFoundJSONResponse: oapi.NotFoundJSONResponse{Error: ae.msg}}, nil
		case http.StatusConflict:
			return oapi.ControlPlan409JSONResponse{Error: ae.msg}, nil
		case http.StatusBadRequest:
			return oapi.ControlPlan400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{Error: ae.msg}}, nil
		}
	}
	return nil, err
}
