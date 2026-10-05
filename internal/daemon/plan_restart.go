package daemon

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os/exec"
	"strconv"
	"strings"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/audit"
	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/daemon/oapi"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/planstore"
	"github.com/srjn45/warden/internal/store"
)

var (
	_ autopilot.RestartRuntime       = autopilotRuntime{}
	_ autopilot.RestartProberFactory = autopilotRuntime{}
)

// RestartPlan implements POST /api/v1/plans/{plan_id}/restart: a work-preserving
// restart of an in_progress plan's executor with a brand-new agent set
// (docs/specs/2026-10-05-plan-restart-and-watchdog.md §1, §3).
func (s *Server) RestartPlan(ctx context.Context, req oapi.RestartPlanRequestObject) (oapi.RestartPlanResponseObject, error) {
	svc := s.planSvc()
	if svc == nil {
		return nil, planNotConfigured()
	}
	var body oapi.RestartPlanRequest
	if req.Body != nil {
		body = *req.Body
	}
	p, err := svc.Get(ctx, req.PlanId)
	if err != nil {
		if errors.Is(err, planstore.ErrNotFound) {
			return oapi.RestartPlan404JSONResponse{NotFoundJSONResponse: oapi.NotFoundJSONResponse{Error: "plan not found"}}, nil
		}
		return nil, errStatus(http.StatusInternalServerError, "get plan: "+err.Error())
	}
	if p.Status != planstore.PlanStatusInProgress {
		return oapi.RestartPlan409JSONResponse{Error: "plan is not in_progress"}, nil
	}

	mode := planExecutionMode(p)
	force := body.Force
	backend := strings.TrimSpace(body.Backend)

	var res autopilot.RestartResult
	switch mode {
	case planstore.PlanModeAutopilot:
		if p.AutopilotRunID == "" || s.autopilot == nil {
			return oapi.RestartPlan409JSONResponse{Error: "plan has no active executor to restart"}, nil
		}
		res, err = s.autopilot.RestartRun(ctx, p.AutopilotRunID, autopilot.RestartRequest{Force: force, Backend: backend})
		if err != nil {
			return mapPlanRestartErr(err)
		}
	case planstore.PlanModePipeline:
		return s.restartPlanPipeline(ctx, p, force)
	case "":
		return oapi.RestartPlan409JSONResponse{Error: "plan has no active executor to restart"}, nil
	default:
		return oapi.RestartPlan409JSONResponse{Error: "restart is not supported for " + string(mode) + " plans; use stop and a new run path"}, nil
	}

	updated, err := svc.Get(ctx, req.PlanId)
	if err != nil {
		return nil, errStatus(http.StatusInternalServerError, "fetch updated plan: "+err.Error())
	}
	s.recordAuditCtx(ctx, audit.ActionPlanRestart, req.PlanId, map[string]string{
		"plan_id":          req.PlanId,
		"mode":             string(mode),
		"reason_kind":      res.ReasonKind,
		"force":            strconv.FormatBool(force),
		"agents_removed":   strconv.Itoa(res.AgentsRemoved),
		"branches_kept":    strconv.Itoa(len(res.BranchesKept)),
		"branches_deleted": strconv.Itoa(len(res.BranchesDeleted)),
		"backend":          res.Backend,
		"restart_count":    strconv.Itoa(res.RestartCount),
	})
	return oapi.RestartPlan200JSONResponse(s.planToOAPI(updated)), nil
}

// restartPlanPipeline is the pipeline-mode branch of RestartPlan (spec §3.2):
// reset the plan's pipeline via Executor.RestartPipeline, sync plan task
// progress, then reconcile so ready jobs spawn with fresh agents.
func (s *Server) restartPlanPipeline(ctx context.Context, p *planstore.Plan, force bool) (oapi.RestartPlanResponseObject, error) {
	if s.exec == nil || p.PipelineID == "" {
		return oapi.RestartPlan409JSONResponse{Error: "plan has no active executor to restart"}, nil
	}
	in := pipelineRestartInput{Force: force, CtxKey: autopilot.PlanRestartContextKey(p.ID)}
	if s.cstore != nil {
		in.Store = ctxLedgerStore{cs: s.cstore}
	}
	res, err := s.exec.RestartPipeline(ctx, p.PipelineID, in)
	switch {
	case errors.Is(err, pipeline.ErrNotFound):
		return oapi.RestartPlan404JSONResponse{NotFoundJSONResponse: oapi.NotFoundJSONResponse{Error: "pipeline not found"}}, nil
	case errors.Is(err, ErrPipelineRestartRefused):
		return oapi.RestartPlan409JSONResponse{Error: err.Error()}, nil
	case errors.Is(err, errPipelineComplete):
		return oapi.RestartPlan409JSONResponse{Error: "complete run is terminal"}, nil
	case err != nil:
		return nil, errStatus(http.StatusInternalServerError, err.Error())
	}
	s.syncPlanProgressAfterRestart(ctx, p.ID, res.ResetJobs)
	if rerr := s.exec.Reconcile(context.Background(), p.PipelineID); rerr != nil {
		return nil, errStatus(http.StatusInternalServerError, "reconcile: "+rerr.Error())
	}
	updated, gerr := s.planSvc().Get(ctx, p.ID)
	if gerr != nil {
		return nil, errStatus(http.StatusInternalServerError, "fetch updated plan: "+gerr.Error())
	}
	s.recordAuditCtx(ctx, audit.ActionPlanRestart, p.ID, map[string]string{
		"plan_id":          p.ID,
		"mode":             string(planstore.PlanModePipeline),
		"reason_kind":      res.ReasonKind,
		"force":            strconv.FormatBool(force),
		"agents_removed":   strconv.Itoa(res.AgentsRemoved),
		"branches_kept":    strconv.Itoa(len(res.BranchesKept)),
		"branches_deleted": strconv.Itoa(len(res.BranchesDeleted)),
		"restart_count":    strconv.Itoa(res.RestartCount),
	})
	return oapi.RestartPlan200JSONResponse(s.planToOAPI(updated)), nil
}

// syncPlanProgressAfterRestart resets the plan's task progress for every reset
// job to pending, drops their stale outcomes and reopens the sealed
// ActiveExecution (a stopped pipeline sealed it "cancelled").
func (s *Server) syncPlanProgressAfterRestart(ctx context.Context, planID string, resetJobs []string) {
	if s.plans == nil {
		return
	}
	err := s.plans.Update(ctx, planID, func(up *planstore.Plan) error {
		if up.TaskProgress == nil {
			up.TaskProgress = map[string]string{}
		}
		for _, jobID := range resetJobs {
			taskID := resolvePlanTaskID(up, jobID)
			up.TaskProgress[taskID] = "pending"
			delete(up.TaskOutcomes, taskID)
			if up.ActiveExecution != nil && up.ActiveExecution.TaskProgress != nil {
				up.ActiveExecution.TaskProgress[taskID] = "pending"
			}
		}
		if ae := up.ActiveExecution; ae != nil {
			ae.TerminalStatus = planstore.ExecutionStatusRunning
			ae.CompletedAt = nil
		}
		return nil
	})
	if err != nil {
		slog.Warn("plan restart: progress sync failed", "plan", planID, "err", err)
	}
}

func planExecutionMode(p *planstore.Plan) planstore.PlanExecutionMode {
	switch {
	case p.AutopilotRunID != "":
		return planstore.PlanModeAutopilot
	case p.PipelineID != "":
		return planstore.PlanModePipeline
	case p.ActiveExecution != nil && p.ActiveExecution.ExecutionMode != "":
		return p.ActiveExecution.ExecutionMode
	}
	return ""
}

func mapPlanRestartErr(err error) (oapi.RestartPlanResponseObject, error) {
	switch {
	case errors.Is(err, autopilot.ErrRunNotFound):
		return oapi.RestartPlan404JSONResponse{NotFoundJSONResponse: oapi.NotFoundJSONResponse{Error: err.Error()}}, nil
	case errors.Is(err, autopilot.ErrRunConflict):
		return oapi.RestartPlan409JSONResponse{Error: strings.TrimPrefix(err.Error(), autopilot.ErrRunConflict.Error()+": ")}, nil
	}
	return nil, errStatus(http.StatusInternalServerError, err.Error())
}

// TeardownRunAgents terminates every agent tagged run:<runID> (manager and
// workers), removes their worktrees and applies the restart branch policy: a
// branch with commits beyond the integration branch is kept (PRs untouched), an
// empty one is deleted. Idempotent.
func (rt autopilotRuntime) TeardownRunAgents(ctx context.Context, runID, repo, integrationBranch string) (autopilot.RestartTeardown, error) {
	var td autopilot.RestartTeardown
	all, err := rt.s.store.List(ctx)
	if err != nil {
		return td, err
	}
	var errs []error
	for _, sess := range all {
		if sess == nil || autopilot.SessionRunID(sess) != runID {
			continue
		}
		if err := rt.teardownRunAgent(ctx, sess, repo, integrationBranch, &td); err != nil {
			errs = append(errs, err)
			continue
		}
		td.AgentsRemoved++
	}
	rt.s.notify()
	return td, errors.Join(errs...)
}

func (rt autopilotRuntime) teardownRunAgent(ctx context.Context, sess *agentstore.Agent, repo, integ string, td *autopilot.RestartTeardown) error {
	if sess.TmuxSession != "" {
		_ = rt.s.life.Terminate(ctx, sess.TmuxSession) // idempotent; a gone session is fine
	}
	_ = rt.s.store.UpdateStatus(ctx, sess.ID, store.StatusDone)
	rt.s.recordPlanBoundAgentFinished(sess, "plan_restart")
	if sess.Worktree != "" {
		branch := sess.Branch
		wt := *sess
		wt.BranchCreated = false // RemoveWorktree must not -D; the branch policy below decides
		if err := rt.s.life.RemoveWorktree(ctx, &wt, true, false); err != nil {
			return err
		}
		rt.s.recordPlanBoundWorktreeRemoved(sess)
		if branch != "" && branch != integ {
			rt.applyBranchPolicy(ctx, sess, repo, integ, td)
		}
	}
	if err := rt.s.store.Archive(ctx, sess.ID); err != nil && !errors.Is(err, agentstore.ErrNotFound) {
		return err
	}
	rt.s.removeProjectMembership(sess)
	return nil
}

func (rt autopilotRuntime) applyBranchPolicy(ctx context.Context, sess *agentstore.Agent, repo, integ string, td *autopilot.RestartTeardown) {
	if repo == "" {
		repo = sess.Repo
	}
	facts, err := (restartGitProber{repo: repo}).BranchFacts(ctx, sess.Branch, integ)
	if err != nil || facts.HasUnmergedCommits {
		td.BranchesKept = append(td.BranchesKept, sess.Branch) // unknown ⇒ keep
		return
	}
	if out, derr := gitOut(ctx, repo, "branch", "-D", sess.Branch); derr != nil {
		if !strings.Contains(out, "not found") {
			td.BranchesKept = append(td.BranchesKept, sess.Branch)
			return
		}
	}
	if sess.BranchCreated {
		_, _ = gitOut(ctx, repo, "push", "--no-verify", "origin", "--delete", sess.Branch) // best-effort; PRs are never closed
	}
	td.BranchesDeleted = append(td.BranchesDeleted, sess.Branch)
}

// RestartProber returns the git/gh-backed branch prober bound to repo.
func (rt autopilotRuntime) RestartProber(repo string) autopilot.RestartBranchProber {
	return restartGitProber{repo: repo}
}

// restartGitProber answers BranchFacts with git rev-list and (best-effort) gh.
type restartGitProber struct{ repo string }

func (p restartGitProber) BranchFacts(ctx context.Context, branch, integ string) (autopilot.BranchFacts, error) {
	var f autopilot.BranchFacts
	out, err := gitOut(ctx, p.repo, "rev-list", "--count", integ+".."+branch)
	if err != nil {
		return f, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return f, err
	}
	f.HasUnmergedCommits = n > 0
	if ghOut, gerr := ghOut(ctx, p.repo, "pr", "list", "--head", branch, "--state", "open",
		"--json", "number,baseRefName", "--jq", `.[0] | "\(.number) \(.baseRefName)"`); gerr == nil {
		if parts := strings.Fields(ghOut); len(parts) == 2 {
			if num, nerr := strconv.Atoi(parts[0]); nerr == nil {
				f.OpenPR, f.PRBase = num, parts[1]
			}
		}
	}
	return f, nil
}

func gitOut(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func ghOut(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return string(out), err
}
