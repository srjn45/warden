package daemon

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/planstore"
)

// startPlanAutopilotExecution creates the live Autopilot + manager Agent for
// Plan run mode=autopilot, records ActiveExecution (with snapshot-at-start),
// and appends typed PlanExecutionEvents. Plan definition comes from ScrivaDB
// only — no repository YAML is read.
func (s *Server) startPlanAutopilotExecution(ctx context.Context, p *planstore.Plan, root string) (string, []string, error) {
	if s.autopilot == nil {
		return "", nil, errStatus(http.StatusServiceUnavailable, "autopilot not configured")
	}
	if strings.TrimSpace(p.ID) == "" {
		return "", nil, errStatus(http.StatusBadRequest, "autopilot requires a plan_id")
	}

	snap := planstore.SnapshotFromPlan(p)
	res, err := s.autopilot.StartFromPlan(ctx, autopilot.PlanStartRequest{
		PlanID:     p.ID,
		ProjectID:  p.ProjectID,
		Name:       p.Name,
		Repo:       root,
		Definition: snap,
		// PlanFile is optional last-export metadata only; never an execution input.
		PlanFile: strings.TrimSpace(p.FilePath),
	})
	if err != nil {
		if err == autopilot.ErrPlanIDRequired {
			return "", nil, errStatus(http.StatusBadRequest, err.Error())
		}
		return "", nil, errStatus(http.StatusInternalServerError, "start autopilot from plan: "+err.Error())
	}

	s.addAutopilotMembership(res.AutopilotID, p.ProjectID)
	s.addPlanMembership(p.ID, p.ProjectID)

	if err := s.beginPlanAutopilotExecution(ctx, p, snap, res.AutopilotID, res.ManagerAgentID); err != nil {
		return res.AutopilotID, nil, err
	}
	// Seed outcome.integration_branch / default_branch (plan-finish-flow §6).
	integ, def := res.Status.IntegrationBranch, ""
	if lp, ok := s.autopilot.LandParams(res.AutopilotID); ok {
		if integ == "" {
			integ = lp.IntegrationBranch
		}
		def = lp.DefaultBranch
	}
	s.seedPlanOutcome(ctx, p.ID, integ, def)
	return res.AutopilotID, runWarnings(res.Status), nil
}

// beginPlanAutopilotExecution stamps ActiveExecution (with snapshot) and
// appends execution_started / executor_created / agent_spawned events.
func (s *Server) beginPlanAutopilotExecution(ctx context.Context, p *planstore.Plan, snap *planstore.ExecutionSnapshot, autopilotID, managerID string) error {
	if s.plans == nil || p == nil {
		return nil
	}
	if snap == nil {
		snap = planstore.SnapshotFromPlan(p)
	}
	execID := planstore.NewPlanExecutionID()
	now := time.Now().UTC()
	tasksTotal := 0
	if snap != nil {
		tasksTotal = len(snap.Tasks)
	}
	pe := planstore.PlanExecution{
		ID:             execID,
		PlanID:         p.ID,
		ExecutionMode:  planstore.PlanModeAutopilot,
		ExecutorID:     autopilotID,
		StartedAt:      now,
		TerminalStatus: planstore.ExecutionStatusRunning,
		Snapshot:       snap,
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

// runWarnings collects the operator-visible, non-blocking notes of a freshly
// started run: the CI-coverage gate downgrade and any preflight warnings.
func runWarnings(st autopilot.RunStatus) []string {
	var out []string
	if st.GateWarning != "" {
		out = append(out, st.GateWarning)
	}
	return append(out, st.PreflightWarnings...)
}
