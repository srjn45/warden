package daemon

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/planstore"
)

// startPlanAutopilotExecution creates the live Autopilot + manager Agent for
// Plan run mode=autopilot, records ActiveExecution, and appends typed
// PlanExecutionEvents. Plan is the source of task state; the Autopilot is a
// disposable executor (no plan-file-only registration).
func (s *Server) startPlanAutopilotExecution(ctx context.Context, p *planstore.Plan, root string) (string, error) {
	if s.autopilot == nil {
		return "", errStatus(http.StatusServiceUnavailable, "autopilot not configured")
	}
	if strings.TrimSpace(p.ID) == "" {
		return "", errStatus(http.StatusBadRequest, "autopilot requires a plan_id")
	}

	planFile := filepath.Join(root, p.FilePath)
	res, err := s.autopilot.StartFromPlan(ctx, autopilot.PlanStartRequest{
		PlanID:    p.ID,
		ProjectID: p.ProjectID,
		Name:      p.Name,
		Repo:      root,
		PlanFile:  planFile,
	})
	if err != nil {
		if err == autopilot.ErrPlanIDRequired {
			return "", errStatus(http.StatusBadRequest, err.Error())
		}
		return "", errStatus(http.StatusInternalServerError, "start autopilot from plan: "+err.Error())
	}

	s.addAutopilotMembership(res.AutopilotID, p.ProjectID)
	s.addPlanMembership(p.ID, p.ProjectID)

	if err := s.beginPlanAutopilotExecution(ctx, p, res.AutopilotID, res.ManagerAgentID, root); err != nil {
		return res.AutopilotID, err
	}
	return res.AutopilotID, nil
}

// beginPlanAutopilotExecution stamps ActiveExecution and appends
// execution_started / executor_created / agent_spawned events.
func (s *Server) beginPlanAutopilotExecution(ctx context.Context, p *planstore.Plan, autopilotID, managerID, root string) error {
	if s.plans == nil || p == nil {
		return nil
	}
	execID := planstore.NewPlanExecutionID()
	now := time.Now().UTC()
	tasksTotal := 0
	if ap, err := autopilot.LoadPlan(filepath.Join(root, p.FilePath)); err == nil {
		tasksTotal = len(ap.Tasks)
	}
	pe := planstore.PlanExecution{
		ID:             execID,
		PlanID:         p.ID,
		ExecutionMode:  planstore.PlanModeAutopilot,
		ExecutorID:     autopilotID,
		StartedAt:      now,
		TerminalStatus: planstore.ExecutionStatusRunning,
	}
	if err := s.plans.Update(ctx, p.ID, func(pl *planstore.Plan) error {
		pl.ActiveExecution = &pe
		pl.ExecutionMode = planstore.PlanModeAutopilot
		pl.AutopilotRunID = autopilotID
		return nil
	}); err != nil {
		return errStatus(http.StatusInternalServerError, "record plan execution: "+err.Error())
	}

	events := []*planstore.PlanExecutionEvent{
		{
			DedupKey:    fmt.Sprintf("%s:%s:execution_started", p.ID, execID),
			PlanID:      p.ID,
			ExecutionID: execID,
			Kind:        planstore.EventKindExecutionStarted,
			OccurredAt:  now,
			Payload: &planstore.EventPayload{
				ExecutorID:    autopilotID,
				ExecutionMode: string(planstore.PlanModeAutopilot),
				PlanName:      p.Name,
				TasksTotal:    tasksTotal,
				AgentID:       managerID,
			},
		},
		{
			DedupKey:    fmt.Sprintf("%s:%s:executor_created", p.ID, execID),
			PlanID:      p.ID,
			ExecutionID: execID,
			Kind:        planstore.EventKindExecutorCreated,
			OccurredAt:  now,
			Payload: &planstore.EventPayload{
				ExecutorID:    autopilotID,
				ExecutionMode: string(planstore.PlanModeAutopilot),
				AgentID:       managerID,
			},
		},
	}
	if managerID != "" {
		events = append(events, &planstore.PlanExecutionEvent{
			DedupKey:    fmt.Sprintf("%s:%s:agent_spawned:%s", p.ID, execID, managerID),
			PlanID:      p.ID,
			ExecutionID: execID,
			Kind:        planstore.EventKindAgentSpawned,
			OccurredAt:  now,
			Payload: &planstore.EventPayload{
				AgentID:    managerID,
				ExecutorID: autopilotID,
			},
		})
	}
	for _, ev := range events {
		if err := s.plans.AppendEvent(ctx, ev); err != nil {
			return errStatus(http.StatusInternalServerError, "append plan event: "+err.Error())
		}
	}
	return nil
}
