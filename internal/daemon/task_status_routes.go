package daemon

import (
	"context"
	"net/http"

	"github.com/srjn45/warden/internal/daemon/oapi"
)

func (s *Server) UpdateAutopilotTaskStatus(ctx context.Context, req oapi.UpdateAutopilotTaskStatusRequestObject) (oapi.UpdateAutopilotTaskStatusResponseObject, error) {
	if s.autopilot == nil {
		return oapi.UpdateAutopilotTaskStatus403JSONResponse{Error: autopilotDisabledMsg}, nil
	}
	caller := s.callerSession(ctx)
	if caller == nil || caller.Role != autopilotBrainRole {
		return oapi.UpdateAutopilotTaskStatus403JSONResponse{Error: "only an autopilot brain may update task status"}, nil
	}
	if req.Body == nil || req.Body.RunId == "" || req.Body.TaskId == "" {
		return nil, errStatus(http.StatusBadRequest, "run_id and task_id are required")
	}
	if runIDFromTags(caller.Tags) != req.Body.RunId {
		return oapi.UpdateAutopilotTaskStatus403JSONResponse{Error: "brain may only update its own run"}, nil
	}
	activeBrainID, ok := s.autopilot.ActiveBrainForRun(req.Body.RunId)
	if !ok || activeBrainID != caller.ID {
		return oapi.UpdateAutopilotTaskStatus403JSONResponse{Error: "only the run's active brain may update task status"}, nil
	}
	pr := req.Body.LandedPr
	task, err := s.autopilot.UpdateTaskStatus(req.Body.RunId, req.Body.TaskId, string(req.Body.Status), pr)
	if err != nil {
		return oapi.UpdateAutopilotTaskStatus409JSONResponse{Error: err.Error()}, nil
	}
	return oapi.UpdateAutopilotTaskStatus200JSONResponse{Id: task.ID, Prompt: task.Prompt, After: task.After, Status: oapi.AutopilotPlanTaskStatus(task.Status), LandedPr: task.LandedPR}, nil
}
