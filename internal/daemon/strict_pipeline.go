package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/srjn45/warden/internal/audit"
	"github.com/srjn45/warden/internal/daemon/oapi"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/planstore"
)

// ListPipelines implements GET /api/v1/pipelines.
func (s *Server) ListPipelines(_ context.Context, req oapi.ListPipelinesRequestObject) (oapi.ListPipelinesResponseObject, error) {
	ps, err := s.exec.pstore.List()
	if err != nil {
		return nil, err
	}
	projectID := strings.TrimSpace(req.Params.ProjectId)
	statuses := map[string]bool{}
	for _, st := range strings.Split(req.Params.Status, ",") {
		if st = strings.TrimSpace(st); st != "" {
			statuses[st] = true
		}
	}
	out := make([]oapi.Pipeline, 0, len(ps))
	for _, p := range ps {
		if projectID != "" && p.ProjectID != projectID {
			continue
		}
		if len(statuses) > 0 && !statuses[string(p.Status)] {
			continue
		}
		out = append(out, *p)
	}
	return oapi.ListPipelines200JSONResponse{Pipelines: out}, nil
}

// CreatePipeline implements POST /api/v1/pipelines.
func (s *Server) CreatePipeline(ctx context.Context, req oapi.CreatePipelineRequestObject) (oapi.CreatePipelineResponseObject, error) {
	spec := ""
	bodyProjectID := ""
	bodyParentAgentID := ""
	bodyPlanID := ""
	if req.Body != nil {
		spec = req.Body.Spec
		bodyProjectID = req.Body.ProjectId
		bodyParentAgentID = req.Body.ParentAgentId
		bodyPlanID = req.Body.PlanId
	}
	p, err := pipeline.ParseSpec([]byte(spec))
	if err != nil {
		return nil, errStatus(http.StatusBadRequest, err.Error())
	}
	// Stamp the owning project (spec D2/§3.1). Precedence: an explicit request-body
	// project_id wins over any project_id in the YAML spec (already parsed onto
	// p.ProjectID); when both are empty, resolve it by matching the pipeline repo to
	// an OPEN project. No match leaves the pipeline project-less.
	if bodyProjectID != "" {
		p.ProjectID = bodyProjectID
	}
	if p.ProjectID == "" {
		p.ProjectID = s.resolvePipelineProjectID(p)
	} else {
		p.ProjectID = s.ensureExplicitProjectID(p.ProjectID)
	}
	// Optional PlanID back-ref (plan-links-on-agent-pipeline). Empty is always
	// valid; a non-empty value must name a plan in the same project.
	if bodyPlanID != "" {
		p.PlanID = bodyPlanID
	}
	if code, msg := s.validatePlanLink(ctx, p.PlanID, p.ProjectID); code != 0 {
		return nil, errStatus(code, msg)
	}
	// Stamp the owning agent (spec D6/§3.4). Precedence: an explicit request-body
	// parent_agent_id wins over the actor identity; when both are empty the pipeline
	// is operator-created and owns no agent edge. Resolved from the live request
	// before the async executor takes over, since jobs spawn later where no actor
	// identity exists. A terminal can never own a pipeline (§6.4): an explicit
	// override naming a terminal is rejected (operator-owned) and must NOT fall
	// back to the actor identity.
	if bodyParentAgentID != "" {
		if s.terminals != nil {
			if _, terr := s.terminals.Get(ctx, bodyParentAgentID); terr == nil {
				p.ParentAgentID = "" // rejected terminal override
			} else {
				p.ParentAgentID = bodyParentAgentID
			}
		} else {
			p.ParentAgentID = bodyParentAgentID
		}
	} else if p.ParentAgentID == "" {
		p.ParentAgentID = s.resolvePipelineParentAgentID(ctx)
	}
	// Captured at creation because jobs spawn later from the executor's ticker,
	// where no request (and so no actor identity) exists anymore.
	p.Tags = s.inheritOwnershipTags(ctx, p.Tags)
	if err := s.exec.pstore.Create(p); errors.Is(err, pipeline.ErrExists) {
		return nil, errStatus(http.StatusConflict, "pipeline "+p.ID+" already exists")
	} else if err != nil {
		return nil, err
	}
	// Append to the project's authoritative pipelines[] membership list (best-effort).
	s.addPipelineMembership(p)
	// Wire the owning-agent forward edge (spec D4/§6.1): a pipeline created under an
	// agent is appended to that agent's ChildPipelines[]. No-op for an
	// operator-created pipeline (empty ParentAgentID).
	s.addPipelineParentEdge(ctx, p)
	return oapi.CreatePipeline201JSONResponse(*p), nil
}

// GetPipeline implements GET /api/v1/pipelines/{pid}.
func (s *Server) GetPipeline(_ context.Context, req oapi.GetPipelineRequestObject) (oapi.GetPipelineResponseObject, error) {
	p, err := s.exec.pstore.Get(req.Pid)
	if errors.Is(err, pipeline.ErrNotFound) {
		return nil, errStatus(http.StatusNotFound, "pipeline not found")
	}
	if err != nil {
		return nil, err
	}
	return oapi.GetPipeline200JSONResponse(*p), nil
}

// DeletePipeline implements DELETE /api/v1/pipelines/{pid}. It refuses while any
// job is still live so live agents are never orphaned.
func (s *Server) DeletePipeline(ctx context.Context, req oapi.DeletePipelineRequestObject) (oapi.DeletePipelineResponseObject, error) {
	pid := req.Pid
	p, err := s.exec.pstore.Get(pid)
	if errors.Is(err, pipeline.ErrNotFound) {
		return nil, errStatus(http.StatusNotFound, "pipeline not found")
	}
	if err != nil {
		return nil, err
	}
	if err := s.refusePlanOwnedPipeline(ctx, p, "delete"); err != nil {
		return nil, err
	}
	for i := range p.Jobs {
		if p.Jobs[i].Status == pipeline.JobRunning || p.Jobs[i].Status == pipeline.JobNeedsAttention {
			return nil, errStatus(http.StatusConflict, "pipeline has live jobs — cancel it first")
		}
	}
	// Reap each settled job's agent session so deleting never orphans agents.
	for i := range p.Jobs {
		if agentID := p.Jobs[i].AgentRef(); agentID != "" {
			sess, gerr := s.store.Get(ctx, agentID)
			_ = s.life.Terminate(ctx, agentID)
			if aerr := s.store.Archive(ctx, agentID); aerr == nil && gerr == nil {
				s.removeProjectMembership(sess)
			}
		}
	}
	if err := s.exec.pstore.Delete(pid); err != nil {
		return nil, err
	}
	// Drop this pipeline from its project's pipelines[] membership list (best-effort).
	s.removePipelineMembership(p)
	// Drop this pipeline from its owning agent's ChildPipelines[] forward edge — the
	// delete-side mirror of the create-time add (spec D4/§6.1). No-op for an
	// operator-created pipeline.
	s.removePipelineParentEdge(ctx, p)
	// Clear this pipeline's shared-context keys (best-effort).
	if s.exec.cstore != nil {
		_, _ = s.exec.cstore.DelPrefix("pipeline." + pid + ".")
	}
	s.notify()
	return oapi.DeletePipeline200JSONResponse{OKJSONResponse: oapi.OKJSONResponse{Status: "deleted"}}, nil
}

// StartPipeline implements POST /api/v1/pipelines/{pid}/start.
func (s *Server) StartPipeline(ctx context.Context, req oapi.StartPipelineRequestObject) (oapi.StartPipelineResponseObject, error) {
	pid := req.Pid
	p, err := s.exec.pstore.Get(pid)
	if errors.Is(err, pipeline.ErrNotFound) {
		return nil, errStatus(http.StatusNotFound, "pipeline not found")
	}
	if err != nil {
		return nil, err
	}
	if p.Status != pipeline.StatusPending {
		return nil, errStatus(http.StatusConflict, "pipeline already started (status "+string(p.Status)+")")
	}
	if err := s.exec.pstore.Update(pid, func(p *pipeline.Pipeline) { p.Status = pipeline.StatusRunning }); err != nil {
		return nil, err
	}
	// Reconcile on a daemon-owned context: spawning worktree jobs can outlast the
	// triggering request, and DAG progress must not be tied to it (spec §15).
	if err := s.exec.Reconcile(context.Background(), pid); err != nil {
		return nil, err
	}
	s.recordAuditCtx(ctx, audit.ActionPipelineStart, pid, map[string]string{"name": p.Name})
	return oapi.StartPipeline200JSONResponse{OKJSONResponse: oapi.OKJSONResponse{Status: "started"}}, nil
}

// PausePipeline implements POST /api/v1/pipelines/{pid}/pause.
func (s *Server) PausePipeline(ctx context.Context, req oapi.PausePipelineRequestObject) (oapi.PausePipelineResponseObject, error) {
	if err := s.guardPlanOwnedByID(ctx, req.Pid, "pause"); err != nil {
		return nil, err
	}
	switch err := s.exec.Pause(req.Pid); {
	case errors.Is(err, pipeline.ErrNotFound):
		return nil, errStatus(http.StatusNotFound, "pipeline not found")
	case errors.Is(err, ErrNotPausable):
		return nil, errStatus(http.StatusConflict, err.Error())
	case err != nil:
		return nil, err
	}
	s.notify()
	return oapi.PausePipeline200JSONResponse{OKJSONResponse: oapi.OKJSONResponse{Status: "paused"}}, nil
}

// ResumePipeline implements POST /api/v1/pipelines/{pid}/resume.
func (s *Server) ResumePipeline(ctx context.Context, req oapi.ResumePipelineRequestObject) (oapi.ResumePipelineResponseObject, error) {
	if err := s.guardPlanOwnedByID(ctx, req.Pid, "resume"); err != nil {
		return nil, err
	}
	switch err := s.exec.Resume(context.Background(), req.Pid); {
	case errors.Is(err, pipeline.ErrNotFound):
		return nil, errStatus(http.StatusNotFound, "pipeline not found")
	case errors.Is(err, ErrNotPaused):
		return nil, errStatus(http.StatusConflict, err.Error())
	case err != nil:
		return nil, err
	}
	return oapi.ResumePipeline200JSONResponse{OKJSONResponse: oapi.OKJSONResponse{Status: "resumed"}}, nil
}

// CancelPipeline implements POST /api/v1/pipelines/{pid}/cancel.
func (s *Server) CancelPipeline(ctx context.Context, req oapi.CancelPipelineRequestObject) (oapi.CancelPipelineResponseObject, error) {
	if err := s.guardPlanOwnedByID(ctx, req.Pid, "cancel"); err != nil {
		return nil, err
	}
	if err := s.cancelPipeline(ctx, req.Pid); err != nil {
		return nil, err
	}
	return oapi.CancelPipeline200JSONResponse{OKJSONResponse: oapi.OKJSONResponse{Status: "canceled"}}, nil
}

// cancelPipeline is the unguarded cancel used by the REST handler (after the
// plan-ownership check) and by the plan stop path.
func (s *Server) cancelPipeline(ctx context.Context, pid string) error {
	p, err := s.exec.pstore.Get(pid)
	if errors.Is(err, pipeline.ErrNotFound) {
		return errStatus(http.StatusNotFound, "pipeline not found")
	}
	if err != nil {
		return err
	}
	if !p.IsCancelable() {
		return errStatus(http.StatusConflict, "pipeline already finished (status "+string(p.Status)+") — nothing to cancel; delete it instead")
	}
	for i := range p.Jobs {
		j := &p.Jobs[i]
		if (j.Status == pipeline.JobRunning || j.Status == pipeline.JobNeedsAttention) && j.AgentRef() != "" {
			_ = s.life.Terminate(ctx, j.AgentRef())
		}
	}
	if err := s.exec.pstore.Update(pid, func(p *pipeline.Pipeline) {
		for i := range p.Jobs {
			switch p.Jobs[i].Status {
			case pipeline.JobPending, pipeline.JobRunning, pipeline.JobNeedsAttention:
				p.Jobs[i].Status = pipeline.JobSkipped
			}
		}
		p.Status = pipeline.StatusCanceled
	}); err != nil {
		return err
	}
	s.notify()
	s.recordAuditCtx(ctx, audit.ActionPipelineCancel, pid, map[string]string{"name": p.Name})
	return nil
}

// EmitPipelineJob implements POST /api/v1/pipelines/{pid}/jobs/{job}/emit.
func (s *Server) EmitPipelineJob(_ context.Context, req oapi.EmitPipelineJobRequestObject) (oapi.EmitPipelineJobResponseObject, error) {
	text := ""
	if req.Body != nil {
		text = req.Body.Text
	}
	if text == "" {
		return nil, errStatus(http.StatusBadRequest, "empty emit text")
	}
	// Background context: emit can trigger a reconcile that spawns dependents.
	switch err := s.exec.Emit(context.Background(), req.Pid, req.Job, text); {
	case errors.Is(err, pipeline.ErrNotFound):
		return nil, errStatus(http.StatusNotFound, "pipeline not found")
	case errors.Is(err, ErrJobNotFound):
		return nil, errStatus(http.StatusNotFound, "job not found")
	case errors.Is(err, ErrJobNotRunning):
		return nil, errStatus(http.StatusConflict, err.Error())
	case err != nil:
		return nil, err
	}
	return oapi.EmitPipelineJob200JSONResponse{OKJSONResponse: oapi.OKJSONResponse{Status: "emitted"}}, nil
}

// EditPipelineJob implements POST /api/v1/pipelines/{pid}/jobs/{job}/edit. A nil
// prompt/handoff means "leave unchanged"; non-nil (incl. "") sets the value.
func (s *Server) EditPipelineJob(_ context.Context, req oapi.EditPipelineJobRequestObject) (oapi.EditPipelineJobResponseObject, error) {
	var prompt, handoff *string
	if req.Body != nil {
		prompt, handoff = req.Body.Prompt, req.Body.Handoff
	}
	if prompt == nil && handoff == nil {
		return nil, errStatus(http.StatusBadRequest, "nothing to edit (provide prompt and/or handoff)")
	}
	if serr := s.guardSyntheticJob(req.Pid, req.Job, "edited"); serr != nil {
		return nil, serr
	}
	switch err := s.exec.EditJob(req.Pid, req.Job, prompt, handoff); {
	case errors.Is(err, pipeline.ErrNotFound):
		return nil, errStatus(http.StatusNotFound, "pipeline not found")
	case errors.Is(err, ErrJobNotFound):
		return nil, errStatus(http.StatusNotFound, "job not found")
	case errors.Is(err, ErrJobNotPending):
		return nil, errStatus(http.StatusConflict, "job is not pending (can only edit before it starts)")
	case err != nil:
		return nil, err
	}
	return oapi.EditPipelineJob200JSONResponse{OKJSONResponse: oapi.OKJSONResponse{Status: "edited"}}, nil
}

// RetryPipelineJob implements POST /api/v1/pipelines/{pid}/jobs/{job}/retry.
func (s *Server) RetryPipelineJob(_ context.Context, req oapi.RetryPipelineJobRequestObject) (oapi.RetryPipelineJobResponseObject, error) {
	if serr := s.guardSyntheticJob(req.Pid, req.Job, "retried"); serr != nil {
		return nil, serr
	}
	// Background context: retry reconciles and may spawn worktree jobs.
	switch err := s.exec.Retry(context.Background(), req.Pid, req.Job); {
	case errors.Is(err, pipeline.ErrNotFound):
		return nil, errStatus(http.StatusNotFound, "pipeline not found")
	case errors.Is(err, ErrJobNotFound):
		return nil, errStatus(http.StatusNotFound, "job not found")
	case errors.Is(err, ErrJobNotRetryable):
		return nil, errStatus(http.StatusConflict, "job is not failed or needs-attention")
	case err != nil:
		return nil, err
	}
	return oapi.RetryPipelineJob200JSONResponse{OKJSONResponse: oapi.OKJSONResponse{Status: "retrying"}}, nil
}

// guardSyntheticJob refuses edit/retry on jobs warden injected itself. A missing
// pipeline or job falls through so the normal 404 handling applies.
func (s *Server) guardSyntheticJob(pid, jobID, verb string) error {
	p, err := s.exec.pstore.Get(pid)
	if err != nil {
		return nil
	}
	if j := p.Job(jobID); j != nil && j.IsSynthetic() {
		return errStatus(http.StatusBadRequest, fmt.Sprintf("job %q is created by warden and cannot be %s", jobID, verb))
	}
	return nil
}

// planVerbForPipelineVerb maps a direct pipeline control verb to the plan
// command that does the same thing for a plan-owned pipeline.
var planVerbForPipelineVerb = map[string]string{
	"pause":  "wd plan pause %s",
	"resume": "wd plan resume %s",
	"cancel": "wd plan stop %s",
	"delete": "wd plan stop %s` then `wd plan archive %s",
}

// planOwningPipeline returns the in-progress plan that runs p, or nil. A
// pipeline is plan-owned only when the plan's executor link points back at it
// (set by the plan run adapter); a pipeline that merely records a --plan
// back-reference, or whose plan is gone, archived or no longer running, is not.
func (s *Server) planOwningPipeline(ctx context.Context, p *pipeline.Pipeline) *planstore.Plan {
	if p == nil || p.PlanID == "" || s.plans == nil {
		return nil
	}
	pl, err := s.plans.Get(ctx, p.PlanID)
	if err != nil || pl == nil || pl.Status != planstore.PlanStatusInProgress {
		return nil
	}
	if pl.PipelineID != p.ID {
		return nil
	}
	return pl
}

// refusePlanOwnedPipeline returns a 409 naming the plan command to use when p
// is run by a plan.
func (s *Server) refusePlanOwnedPipeline(ctx context.Context, p *pipeline.Pipeline, verb string) error {
	pl := s.planOwningPipeline(ctx, p)
	if pl == nil {
		return nil
	}
	cmd := strings.ReplaceAll(planVerbForPipelineVerb[verb], "%s", pl.ID)
	return errStatus(http.StatusConflict, fmt.Sprintf("pipeline %q is run by plan %s; use `%s`", p.Name, pl.ID, cmd))
}

func (s *Server) guardPlanOwnedByID(ctx context.Context, pid, verb string) error {
	p, err := s.exec.pstore.Get(pid)
	if err != nil {
		return nil // not found etc. is reported by the handler proper
	}
	return s.refusePlanOwnedPipeline(ctx, p, verb)
}
