package daemon

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/srjn45/warden/internal/audit"
	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/daemon/oapi"
	"github.com/srjn45/warden/internal/planstore"
)

func (s *Server) ListAutopilotRuns(_ context.Context, _ oapi.ListAutopilotRunsRequestObject) (oapi.ListAutopilotRunsResponseObject, error) {
	if s.autopilot == nil {
		return oapi.ListAutopilotRuns200JSONResponse{}, nil
	}
	return oapi.ListAutopilotRuns200JSONResponse(s.autopilot.Status().Runs), nil
}

// RegisterAutopilotRun is a deprecated one-release alias. Plan-file registration
// is retired; when plan_file resolves to a PlanID the response names that id and
// points at POST /plans/{plan_id}/run. Otherwise it returns a precise migration
// error.
func (s *Server) RegisterAutopilotRun(ctx context.Context, req oapi.RegisterAutopilotRunRequestObject) (oapi.RegisterAutopilotRunResponseObject, error) {
	if s.autopilot == nil {
		return oapi.RegisterAutopilotRun403JSONResponse{Error: autopilotDisabledMsg}, nil
	}
	if req.Body == nil || strings.TrimSpace(req.Body.PlanFile) == "" {
		return oapi.RegisterAutopilotRun400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{Error: "plan_file is required"}}, nil
	}
	planID, err := s.resolvePlanIDFromPlanFile(ctx, req.Body.PlanFile)
	if err != nil || planID == "" {
		return oapi.RegisterAutopilotRun400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{
			Error: "autopilot register is retired: cannot resolve PlanID from plan_file " +
				strings.TrimSpace(req.Body.PlanFile) +
				" — run `wd plan scan` / `wd plan create` / `wd plan import`, then POST /api/v1/plans/{plan_id}/run",
		}}, nil
	}
	return oapi.RegisterAutopilotRun410JSONResponse{Error: fmt.Sprintf(
		"autopilot register is retired: use POST /api/v1/plans/%s/run (execution_mode=autopilot) instead of registering plan files",
		planID,
	)}, nil
}

// ControlAutopilotRun is a deprecated one-release alias. When a PlanID can be
// resolved from the run it translates start → RunPlan and pause/resume/stop/
// unregister → ControlPlan. Otherwise it returns a precise migration error.
func (s *Server) ControlAutopilotRun(ctx context.Context, req oapi.ControlAutopilotRunRequestObject) (oapi.ControlAutopilotRunResponseObject, error) {
	if s.autopilot == nil {
		return oapi.ControlAutopilotRun403JSONResponse{Error: autopilotDisabledMsg}, nil
	}
	action := string(req.Action)
	switch action {
	case "start", "pause", "resume", "stop", "unregister":
	default:
		return oapi.ControlAutopilotRun400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{
			Error: "action must be start, pause, resume, stop, or unregister",
		}}, nil
	}

	planID, run, err := s.resolvePlanIDFromAutopilotRun(ctx, req.RunId)
	if err != nil {
		if errors.Is(err, autopilot.ErrRunNotFound) {
			return oapi.ControlAutopilotRun404JSONResponse{NotFoundJSONResponse: oapi.NotFoundJSONResponse{Error: "autopilot run not found"}}, nil
		}
		return nil, err
	}
	if planID == "" {
		return oapi.ControlAutopilotRun400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{
			Error: fmt.Sprintf(
				"autopilot run control is retired: cannot resolve PlanID for run %s — recreate via POST /api/v1/plans/{plan_id}/run",
				req.RunId,
			),
		}}, nil
	}

	switch action {
	case "start":
		mode := oapi.RunPlanRequestExecutionModeAutopilot
		if p, gerr := s.plans.Get(ctx, planID); gerr == nil && p != nil && p.ExecutionMode != "" {
			mode = oapi.RunPlanRequestExecutionMode(p.ExecutionMode)
		}
		resp, rerr := s.RunPlan(ctx, oapi.RunPlanRequestObject{
			PlanId: planID,
			Body:   &oapi.RunPlanRequest{ExecutionMode: mode},
		})
		if rerr != nil {
			return nil, rerr
		}
		switch resp.(type) {
		case oapi.RunPlan200JSONResponse:
			st, _ := s.autopilot.LookupRun(req.RunId)
			if st.RunID == "" && run.RunID != "" {
				st = run
			}
			if st.RunID == "" {
				st = autopilot.RunStatus{RunID: req.RunId, PlanID: planID, State: autopilot.StateStarting}
			}
			return oapi.ControlAutopilotRun200JSONResponse(st), nil
		case oapi.RunPlan409JSONResponse:
			// Already in progress — treat as resume of the linked executor.
			ctrl, cerr := s.ControlPlan(ctx, oapi.ControlPlanRequestObject{PlanId: planID, Action: "resume"})
			if cerr != nil {
				return nil, cerr
			}
			if _, ok := ctrl.(oapi.ControlPlan200JSONResponse); ok {
				st, _ := s.autopilot.LookupRun(req.RunId)
				if st.RunID == "" {
					st = autopilot.RunStatus{RunID: req.RunId, PlanID: planID, State: autopilot.StateActive}
				}
				return oapi.ControlAutopilotRun200JSONResponse(st), nil
			}
			return oapi.ControlAutopilotRun410JSONResponse{Error: fmt.Sprintf(
				"autopilot run start is retired: use POST /api/v1/plans/%s/run", planID,
			)}, nil
		case oapi.RunPlan400JSONResponse:
			return oapi.ControlAutopilotRun400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{
				Error: fmt.Sprintf("translated plan run failed for plan_id=%s", planID),
			}}, nil
		case oapi.RunPlan404JSONResponse:
			return oapi.ControlAutopilotRun404JSONResponse{NotFoundJSONResponse: oapi.NotFoundJSONResponse{Error: "plan not found"}}, nil
		default:
			return oapi.ControlAutopilotRun410JSONResponse{Error: fmt.Sprintf(
				"autopilot run start is retired: use POST /api/v1/plans/%s/run", planID,
			)}, nil
		}
	case "unregister":
		action = "stop"
		fallthrough
	default: // pause, resume, stop
		ctrl, cerr := s.ControlPlan(ctx, oapi.ControlPlanRequestObject{PlanId: planID, Action: action})
		if cerr != nil {
			return nil, cerr
		}
		switch ctrl.(type) {
		case oapi.ControlPlan200JSONResponse:
			st, _ := s.autopilot.LookupRun(req.RunId)
			if st.RunID == "" {
				st = autopilot.RunStatus{RunID: req.RunId, PlanID: planID, State: autopilot.StateActive}
				if action == "stop" {
					st.State = autopilot.StateStopped
				}
				if action == "pause" {
					st.State = autopilot.StatePaused
				}
			}
			if req.Action == "unregister" {
				// Best-effort legacy cleanup after stop translation.
				_, _ = s.autopilot.UnregisterRun(ctx, req.RunId)
			}
			s.recordAuditCtx(ctx, audit.ActionAutopilotOn, req.RunId, map[string]string{
				"action":  string(req.Action),
				"plan_id": planID,
				"via":     "deprecated_alias",
			})
			return oapi.ControlAutopilotRun200JSONResponse(st), nil
		case oapi.ControlPlan404JSONResponse:
			return oapi.ControlAutopilotRun404JSONResponse{NotFoundJSONResponse: oapi.NotFoundJSONResponse{Error: "plan not found"}}, nil
		case oapi.ControlPlan409JSONResponse:
			return oapi.ControlAutopilotRun409JSONResponse{Error: fmt.Sprintf(
				"translated plan control (%s) conflict for plan_id=%s", action, planID,
			)}, nil
		case oapi.ControlPlan400JSONResponse:
			return oapi.ControlAutopilotRun400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{
				Error: fmt.Sprintf("translated plan control (%s) failed for plan_id=%s", action, planID),
			}}, nil
		default:
			return oapi.ControlAutopilotRun410JSONResponse{Error: fmt.Sprintf(
				"autopilot run %s is retired: use POST /api/v1/plans/%s/%s", action, planID, action,
			)}, nil
		}
	}
}

func (s *Server) RenameAutopilotRun(ctx context.Context, req oapi.RenameAutopilotRunRequestObject) (oapi.RenameAutopilotRunResponseObject, error) {
	if s.autopilot == nil {
		return oapi.RenameAutopilotRun403JSONResponse{Error: autopilotDisabledMsg}, nil
	}
	if req.Body == nil || strings.TrimSpace(req.Body.Name) == "" {
		return oapi.RenameAutopilotRun400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{Error: "name is required"}}, nil
	}
	r, err := s.autopilot.RenameRun(ctx, req.RunId, req.Body.Name)
	if errors.Is(err, autopilot.ErrRunNotFound) {
		return oapi.RenameAutopilotRun404JSONResponse{NotFoundJSONResponse: oapi.NotFoundJSONResponse{Error: "autopilot run not found"}}, nil
	}
	if errors.Is(err, autopilot.ErrRunConflict) {
		return oapi.RenameAutopilotRun409JSONResponse{Error: err.Error()}, nil
	}
	if err != nil {
		return nil, err
	}
	s.recordAuditCtx(ctx, audit.ActionAutopilotOn, req.RunId, map[string]string{"action": "rename", "name": req.Body.Name})
	return oapi.RenameAutopilotRun200JSONResponse(r), nil
}

// RetargetAutopilotRun is a deprecated one-release alias with no safe PlanID
// translation — always returns 410 with a precise migration error.
func (s *Server) RetargetAutopilotRun(ctx context.Context, req oapi.RetargetAutopilotRunRequestObject) (oapi.RetargetAutopilotRunResponseObject, error) {
	if s.autopilot == nil {
		return oapi.RetargetAutopilotRun403JSONResponse{Error: autopilotDisabledMsg}, nil
	}
	if req.Body == nil {
		return oapi.RetargetAutopilotRun400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{Error: "request body is required"}}, nil
	}
	planID, _, err := s.resolvePlanIDFromAutopilotRun(ctx, req.RunId)
	if errors.Is(err, autopilot.ErrRunNotFound) {
		return oapi.RetargetAutopilotRun404JSONResponse{NotFoundJSONResponse: oapi.NotFoundJSONResponse{Error: "autopilot run not found"}}, nil
	}
	if err != nil {
		return nil, err
	}
	msg := "autopilot retarget is retired: integration branches are owned by Plan-bound Autopilot executors created via POST /api/v1/plans/{plan_id}/run; stop and re-run the plan if a new branch is required"
	if planID != "" {
		msg = fmt.Sprintf(
			"autopilot retarget is retired for plan_id=%s: integration branches are owned by the Plan executor — stop and POST /api/v1/plans/%s/run if a new branch is required",
			planID, planID,
		)
	}
	return oapi.RetargetAutopilotRun410JSONResponse{Error: msg}, nil
}

// resolvePlanIDFromAutopilotRun finds the PlanID stamped on a live/legacy run,
// falling back to matching the run's plan file against planstore.
func (s *Server) resolvePlanIDFromAutopilotRun(ctx context.Context, runID string) (string, autopilot.RunStatus, error) {
	if s.autopilot == nil {
		return "", autopilot.RunStatus{}, autopilot.ErrRunNotFound
	}
	st, err := s.autopilot.LookupRun(runID)
	if err != nil {
		return "", autopilot.RunStatus{}, err
	}
	if strings.TrimSpace(st.PlanID) != "" {
		return st.PlanID, st, nil
	}
	if s.plans != nil && strings.TrimSpace(st.PlanFile) != "" {
		if id, rerr := s.resolvePlanIDFromPlanFile(ctx, st.PlanFile); rerr == nil && id != "" {
			return id, st, nil
		}
	}
	// Plan store may already link AutopilotRunID → Plan.
	if s.plans != nil {
		all, lerr := s.plans.List(ctx)
		if lerr == nil {
			for _, p := range all {
				if p != nil && p.AutopilotRunID == runID {
					return p.ID, st, nil
				}
			}
		}
	}
	return "", st, nil
}

// resolvePlanIDFromPlanFile matches an absolute or relative plan YAML path to a
// planstore record by FilePath.
func (s *Server) resolvePlanIDFromPlanFile(ctx context.Context, planFile string) (string, error) {
	if s.plans == nil {
		return "", errors.New("plan store not configured")
	}
	want := filepath.Clean(strings.TrimSpace(planFile))
	if want == "" {
		return "", errors.New("plan_file is empty")
	}
	absWant := want
	if !filepath.IsAbs(absWant) {
		if a, err := filepath.Abs(absWant); err == nil {
			absWant = a
		}
	}
	baseWant := filepath.Base(want)
	all, err := s.plans.List(ctx)
	if err != nil {
		return "", err
	}
	var match string
	for _, p := range all {
		if p == nil || p.FilePath == "" {
			continue
		}
		fp := filepath.Clean(p.FilePath)
		absFP := fp
		if !filepath.IsAbs(absFP) {
			root := s.resolvePlanRoot(p.ProjectID)
			if root != "" {
				absFP = filepath.Join(root, fp)
			}
		}
		if absFP == absWant || fp == want || filepath.Base(fp) == baseWant {
			if match != "" && match != p.ID {
				return "", fmt.Errorf("ambiguous plan_file match for %s", planFile)
			}
			match = p.ID
		}
	}
	if match == "" {
		return "", planstore.ErrNotFound
	}
	return match, nil
}
