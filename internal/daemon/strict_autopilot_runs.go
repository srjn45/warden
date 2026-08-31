package daemon

import (
	"context"
	"errors"
	"strings"

	"github.com/srjn45/warden/internal/audit"
	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/daemon/oapi"
)

func (s *Server) ListAutopilotRuns(_ context.Context, _ oapi.ListAutopilotRunsRequestObject) (oapi.ListAutopilotRunsResponseObject, error) {
	if s.autopilot == nil {
		return oapi.ListAutopilotRuns200JSONResponse{}, nil
	}
	return oapi.ListAutopilotRuns200JSONResponse(s.autopilot.Status().Runs), nil
}

func (s *Server) RegisterAutopilotRun(ctx context.Context, req oapi.RegisterAutopilotRunRequestObject) (oapi.RegisterAutopilotRunResponseObject, error) {
	if s.autopilot == nil {
		return oapi.RegisterAutopilotRun403JSONResponse{Error: autopilotDisabledMsg}, nil
	}
	if req.Body == nil || strings.TrimSpace(req.Body.PlanFile) == "" {
		return oapi.RegisterAutopilotRun400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{Error: "plan_file is required"}}, nil
	}
	r, err := s.autopilot.Register(ctx, autopilot.RegisterRequest{Name: req.Body.Name, Repo: req.Body.Repo, PlanFile: req.Body.PlanFile})
	if errors.Is(err, autopilot.ErrRunConflict) || errors.Is(err, autopilot.ErrRunExists) {
		return oapi.RegisterAutopilotRun409JSONResponse{Error: err.Error()}, nil
	}
	if err != nil {
		return oapi.RegisterAutopilotRun400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{Error: err.Error()}}, nil
	}
	s.recordAuditCtx(ctx, audit.ActionAutopilotOn, r.RunID, map[string]string{"action": "register"})
	return oapi.RegisterAutopilotRun201JSONResponse(r), nil
}

func (s *Server) ControlAutopilotRun(ctx context.Context, req oapi.ControlAutopilotRunRequestObject) (oapi.ControlAutopilotRunResponseObject, error) {
	if s.autopilot == nil {
		return oapi.ControlAutopilotRun403JSONResponse{Error: autopilotDisabledMsg}, nil
	}
	var r autopilot.RunStatus
	var err error
	switch req.Action {
	case "start":
		r, err = s.autopilot.StartRun(ctx, req.RunId)
	case "pause":
		r, err = s.autopilot.PauseRun(ctx, req.RunId)
	case "resume":
		r, err = s.autopilot.ResumeRun(ctx, req.RunId)
	case "stop":
		r, err = s.autopilot.StopRun(ctx, req.RunId)
	default:
		return oapi.ControlAutopilotRun400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{Error: "action must be start, pause, resume, or stop"}}, nil
	}
	if errors.Is(err, autopilot.ErrRunNotFound) {
		return oapi.ControlAutopilotRun404JSONResponse{NotFoundJSONResponse: oapi.NotFoundJSONResponse{Error: "autopilot run not found"}}, nil
	}
	if errors.Is(err, autopilot.ErrRunConflict) {
		return oapi.ControlAutopilotRun409JSONResponse{Error: err.Error()}, nil
	}
	if err != nil {
		return nil, err
	}
	s.recordAuditCtx(ctx, audit.ActionAutopilotOn, req.RunId, map[string]string{"action": req.Action})
	return oapi.ControlAutopilotRun200JSONResponse(r), nil
}
