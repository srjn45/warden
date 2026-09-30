package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/daemon/oapi"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/planstore"
	"github.com/srjn45/warden/internal/store"
)

// FinalizePlan runs the daemon-owned Plan finalization workflow: reconcile,
// validate CompletionRequirements, persist the immutable ExecutionSummary,
// tear down disposable executors, then move the YAML to plans/completed/.
//
// On partial cleanup failure the Plan stays in_progress with CleanupEvidence
// and the summary is preserved for a retry-safe second call.
func (s *Server) FinalizePlan(ctx context.Context, planID string) (*planstore.FinalizeResult, error) {
	svc := s.planSvc()
	if svc == nil {
		return nil, planNotConfigured()
	}
	cleanup := s.finalizeCleanupHook
	if cleanup == nil {
		cleanup = s.cleanupPlanExecutors
	}
	return svc.Finalize(ctx, planID, cleanup)
}

// cleanupPlanExecutors deletes the live Autopilot / Pipeline / plan-bound root
// Agent and their disposable workers, worktrees, and branches. Preserves PR
// refs on Plan.BranchSummaries / events, ExecutionSummary, PlanExecutionEvents,
// and global audit entries.
func (s *Server) cleanupPlanExecutors(ctx context.Context, p *planstore.Plan) planstore.CleanupEvidence {
	ev := planstore.CleanupEvidence{AttemptedAt: time.Now().UTC()}
	if p == nil {
		return ev
	}

	// Collect plan-bound agents (root + workers) before tearing down executors.
	agents := s.planBoundAgents(ctx, p.ID)

	switch {
	case p.AutopilotRunID != "" || (p.ActiveExecution != nil && p.ActiveExecution.ExecutionMode == planstore.PlanModeAutopilot):
		id := p.AutopilotRunID
		if id == "" && p.ActiveExecution != nil {
			id = p.ActiveExecution.ExecutorID
		}
		if id != "" {
			if err := s.teardownPlanAutopilot(ctx, id, p.ProjectID); err != nil {
				ev.Errors = append(ev.Errors, fmt.Sprintf("autopilot %s: %v", id, err))
				ev.PendingIDs = append(ev.PendingIDs, id)
			} else {
				ev.DeletedIDs = append(ev.DeletedIDs, id)
			}
		}
	case p.PipelineID != "" || (p.ActiveExecution != nil && p.ActiveExecution.ExecutionMode == planstore.PlanModePipeline):
		id := p.PipelineID
		if id == "" && p.ActiveExecution != nil {
			id = p.ActiveExecution.ExecutorID
		}
		if id != "" {
			if err := s.teardownPlanPipeline(ctx, id); err != nil {
				ev.Errors = append(ev.Errors, fmt.Sprintf("pipeline %s: %v", id, err))
				ev.PendingIDs = append(ev.PendingIDs, id)
			} else {
				ev.DeletedIDs = append(ev.DeletedIDs, id)
			}
		}
	}

	for _, a := range agents {
		if err := s.teardownPlanAgent(ctx, a); err != nil {
			ev.Errors = append(ev.Errors, fmt.Sprintf("agent %s: %v", a.ID, err))
			ev.PendingIDs = append(ev.PendingIDs, a.ID)
			continue
		}
		ev.DeletedIDs = append(ev.DeletedIDs, a.ID)
	}

	if svc := s.planSvc(); svc != nil {
		if err := svc.CleanupWorktrees(ctx, p); err != nil {
			ev.WorktreeErrors = append(ev.WorktreeErrors, err.Error())
		}
	}
	return ev
}

func (s *Server) planBoundAgents(ctx context.Context, planID string) []*agentstore.Agent {
	if s.store == nil || strings.TrimSpace(planID) == "" {
		return nil
	}
	all, err := s.store.List(ctx)
	if err != nil {
		return nil
	}
	var out []*agentstore.Agent
	for _, a := range all {
		if a != nil && a.PlanID == planID {
			out = append(out, a)
		}
	}
	return out
}

func (s *Server) teardownPlanAutopilot(ctx context.Context, autopilotID, projectID string) error {
	if s.autopilot == nil {
		return errors.New("autopilot controller not configured")
	}
	if err := s.autopilot.TeardownLive(ctx, autopilotID); err != nil {
		return err
	}
	s.removeAutopilotMembership(autopilotID, projectID)
	return nil
}

func (s *Server) teardownPlanPipeline(ctx context.Context, pipelineID string) error {
	if s.exec == nil || s.exec.pstore == nil {
		return errors.New("pipeline executor not configured")
	}
	p, err := s.exec.pstore.Get(pipelineID)
	if errors.Is(err, pipeline.ErrNotFound) {
		return nil // already gone — idempotent
	}
	if err != nil {
		return err
	}
	// Cancel if still cancelable so jobs settle before delete (mirrors CancelPipeline).
	if p.IsCancelable() {
		for i := range p.Jobs {
			j := &p.Jobs[i]
			if (j.Status == pipeline.JobRunning || j.Status == pipeline.JobNeedsAttention) && j.AgentRef() != "" {
				_ = s.life.Terminate(ctx, j.AgentRef())
			}
		}
		_ = s.exec.pstore.Update(pipelineID, func(pl *pipeline.Pipeline) {
			for i := range pl.Jobs {
				switch pl.Jobs[i].Status {
				case pipeline.JobPending, pipeline.JobRunning, pipeline.JobNeedsAttention:
					pl.Jobs[i].Status = pipeline.JobSkipped
				}
			}
			pl.Status = pipeline.StatusCanceled
		})
		p, _ = s.exec.pstore.Get(pipelineID)
	}
	if p != nil {
		for i := range p.Jobs {
			if agentID := p.Jobs[i].AgentRef(); agentID != "" {
				sess, gerr := s.store.Get(ctx, agentID)
				_ = s.life.Terminate(ctx, agentID)
				if aerr := s.store.Archive(ctx, agentID); aerr == nil && gerr == nil {
					s.removeProjectMembership(sess)
				}
			}
		}
		if err := s.exec.pstore.Delete(pipelineID); err != nil {
			return err
		}
		s.removePipelineMembership(p)
		s.removePipelineParentEdge(ctx, p)
		if s.exec.cstore != nil {
			_, _ = s.exec.cstore.DelPrefix("pipeline." + pipelineID + ".")
		}
	}
	return nil
}

func (s *Server) teardownPlanAgent(ctx context.Context, sess *agentstore.Agent) error {
	if sess == nil || s.life == nil || s.store == nil {
		return nil
	}
	// Already gone from the active store — treat as cleaned.
	if _, err := s.store.Get(ctx, sess.ID); errors.Is(err, agentstore.ErrNotFound) {
		return nil
	}
	if liveStatus(sess.Status) {
		if err := s.life.Terminate(ctx, sess.TmuxSession); err != nil {
			slog.Debug("plan finalize: terminate agent", "agent", sess.ID, "err", err)
		}
		_ = s.store.UpdateStatus(ctx, sess.ID, store.StatusDone)
		s.recordPlanBoundAgentFinished(sess, "plan_finalize")
	}
	if sess.Worktree != "" {
		if err := s.life.RemoveWorktree(ctx, sess, true, true); err != nil {
			return fmt.Errorf("remove worktree: %w", err)
		}
		s.recordPlanBoundWorktreeRemoved(sess)
	}
	if err := s.store.Archive(ctx, sess.ID); err != nil && !errors.Is(err, agentstore.ErrNotFound) {
		return err
	}
	s.removeProjectMembership(sess)
	return nil
}

// CompletePlan implements POST /api/v1/plans/{plan_id}/complete via FinalizePlan.
func (s *Server) CompletePlan(ctx context.Context, req oapi.CompletePlanRequestObject) (oapi.CompletePlanResponseObject, error) {
	if s.planSvc() == nil {
		return nil, planNotConfigured()
	}
	res, err := s.FinalizePlan(ctx, req.PlanId)
	if err != nil {
		if errors.Is(err, planstore.ErrNotFound) {
			return oapi.CompletePlan404JSONResponse{NotFoundJSONResponse: oapi.NotFoundJSONResponse{Error: "plan not found"}}, nil
		}
		if errors.Is(err, planstore.ErrInvalidTransition) {
			return oapi.CompletePlan409JSONResponse{Error: err.Error()}, nil
		}
		var unmet *planstore.UnmetRequirementsError
		if errors.As(err, &unmet) {
			return oapi.CompletePlan422JSONResponse{
				Error:            unmet.Error(),
				IncompleteTasks:  unmet.Requirements.PendingTaskIDs,
				UnmergedBranches: unmet.Requirements.OpenPRBranches,
			}, nil
		}
		var incomplete *planstore.TasksIncompleteError
		if errors.As(err, &incomplete) {
			return oapi.CompletePlan422JSONResponse{Error: incomplete.Error(), IncompleteTasks: incomplete.TaskIDs}, nil
		}
		var unmerged *planstore.BranchesUnmergedError
		if errors.As(err, &unmerged) {
			return oapi.CompletePlan422JSONResponse{Error: unmerged.Error(), UnmergedBranches: unmerged.Branches}, nil
		}
		var cleanup *planstore.CleanupIncompleteError
		if errors.As(err, &cleanup) {
			return nil, errStatus(http.StatusConflict, cleanup.Error())
		}
		return nil, errStatus(http.StatusInternalServerError, "complete plan: "+err.Error())
	}
	return oapi.CompletePlan200JSONResponse(s.planToOAPI(res.Plan)), nil
}
