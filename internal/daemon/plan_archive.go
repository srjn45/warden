package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/daemon/oapi"
	"github.com/srjn45/warden/internal/planstore"
	"github.com/srjn45/warden/internal/store"
)

// liveAutopilotStates are the reported autopilot run states that mean an
// executor is still working (plan-finish-flow §7).
var liveAutopilotStates = map[string]bool{
	"starting": true, "active": true, "paused": true, "healing": true,
	"degraded": true, "finalizing": true, "awaiting_merge": true,
}

// planExecutorLive reports whether the plan still has a live executor and its
// state, for the archive refusal. A stopped or absent executor is not live.
func (s *Server) planExecutorLive(ctx context.Context, p *planstore.Plan) (string, bool) {
	st := s.planExecutorStatus(ctx, p)
	if st == nil {
		return "", false
	}
	switch st.Kind {
	case "autopilot":
		return st.State, liveAutopilotStates[st.State]
	case "pipeline":
		if s.exec != nil && s.exec.pstore != nil {
			if pl, err := s.exec.pstore.Get(p.PipelineID); err == nil && pl != nil && pl.IsCancelable() {
				return st.State, true
			}
		}
		return st.State, false
	default:
		return st.State, liveStatus(store.Status(st.State))
	}
}

// unmergedAgentBranch counts commits on an agent's branch that are not on the
// default branch (0 when merged, gone, or unprobeable plan service).
func (s *Server) unmergedAgentBranch(ctx context.Context, p *planstore.Plan, sess *agentstore.Agent) int {
	svc := s.planSvc()
	if svc == nil || sess == nil || sess.Branch == "" {
		return 0
	}
	root := sess.Repo
	if root == "" {
		return 1 // cannot probe: keep
	}
	return svc.UnmergedCommits(ctx, root, svc.DefaultBranch(ctx, p, root), sess.Branch)
}

// ArchivePlan implements POST /api/v1/plans/{plan_id}/archive.
func (s *Server) ArchivePlan(ctx context.Context, req oapi.ArchivePlanRequestObject) (oapi.ArchivePlanResponseObject, error) {
	svc := s.planSvc()
	if svc == nil {
		return nil, planNotConfigured()
	}
	cur, err := svc.Get(ctx, req.PlanId)
	if err != nil {
		if errors.Is(err, planstore.ErrNotFound) {
			return oapi.ArchivePlan404JSONResponse{NotFoundJSONResponse: oapi.NotFoundJSONResponse{Error: "plan not found"}}, nil
		}
		return nil, errStatus(http.StatusInternalServerError, "get plan: "+err.Error())
	}
	var report *oapi.PlanArchiveReport
	if cur.Status == planstore.PlanStatusInProgress {
		if state, live := s.planExecutorLive(ctx, cur); live {
			return nil, errStatus(http.StatusConflict, fmt.Sprintf(
				"plan %s is still running (executor %s). Stop it first: wd plan stop %s", cur.ID, state, cur.ID))
		}
		report = s.archiveCleanup(ctx, cur)
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
	out := s.planToOAPI(p)
	out.ArchiveReport = report
	return oapi.ArchivePlan200JSONResponse(out), nil
}

// archiveCleanup tears down the leftover executor, plan-bound agents and
// worktrees of a stopped in-progress plan, keeping branches with unmerged
// commits, and summarises the result.
func (s *Server) archiveCleanup(ctx context.Context, p *planstore.Plan) *oapi.PlanArchiveReport {
	ev, rep := s.cleanupPlanExecutorsMode(ctx, p, true)
	out := &oapi.PlanArchiveReport{
		RemovedAgents:   nil,
		RemovedBranches: dedupSorted(rep.RemovedBranches),
		Errors:          append(append([]string(nil), ev.Errors...), ev.WorktreeErrors...),
	}
	for _, id := range ev.DeletedIDs {
		if id == p.AutopilotRunID || id == p.PipelineID {
			out.RemovedExecutor = id
			continue
		}
		out.RemovedAgents = append(out.RemovedAgents, id)
	}
	seen := map[string]bool{}
	for _, k := range rep.KeptBranches {
		if seen[k.Branch] {
			continue
		}
		seen[k.Branch] = true
		out.KeptBranches = append(out.KeptBranches, oapi.PlanArchiveKeptBranch{Branch: k.Branch, Commits: k.Commits})
	}
	return out
}

func dedupSorted(in []string) []string {
	m := map[string]bool{}
	var out []string
	for _, v := range in {
		if !m[v] {
			m[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// UnarchivePlan implements POST /api/v1/plans/{plan_id}/unarchive.
func (s *Server) UnarchivePlan(ctx context.Context, req oapi.UnarchivePlanRequestObject) (oapi.UnarchivePlanResponseObject, error) {
	svc := s.planSvc()
	if svc == nil {
		return nil, planNotConfigured()
	}
	cur, err := svc.Get(ctx, req.PlanId)
	if errors.Is(err, planstore.ErrNotFound) {
		return oapi.UnarchivePlan404JSONResponse{NotFoundJSONResponse: oapi.NotFoundJSONResponse{Error: "plan not found"}}, nil
	}
	if err != nil {
		return nil, errStatus(http.StatusInternalServerError, "get plan: "+err.Error())
	}
	if cur.Status != planstore.PlanStatusArchived {
		return nil, errStatus(http.StatusConflict, fmt.Sprintf("plan %s is %s, not archived; only archived plans can be unarchived", cur.ID, cur.Status))
	}
	p, err := svc.Unarchive(ctx, req.PlanId)
	if err != nil {
		if errors.Is(err, planstore.ErrNotFound) {
			return oapi.UnarchivePlan404JSONResponse{NotFoundJSONResponse: oapi.NotFoundJSONResponse{Error: "plan not found"}}, nil
		}
		if errors.Is(err, planstore.ErrInvalidTransition) {
			return nil, errStatus(http.StatusConflict, err.Error())
		}
		return nil, errStatus(http.StatusInternalServerError, "unarchive plan: "+err.Error())
	}
	return oapi.UnarchivePlan200JSONResponse(s.planToOAPI(p)), nil
}
