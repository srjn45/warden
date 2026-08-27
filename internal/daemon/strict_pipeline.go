package daemon

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/srjn45/warden/internal/audit"
	"github.com/srjn45/warden/internal/daemon/oapi"
	"github.com/srjn45/warden/internal/pipeline"
)

// ListPipelines implements GET /api/v1/pipelines.
func (s *Server) ListPipelines(_ context.Context, _ oapi.ListPipelinesRequestObject) (oapi.ListPipelinesResponseObject, error) {
	ps, err := s.exec.pstore.List()
	if err != nil {
		return nil, err
	}
	out := make([]oapi.Pipeline, 0, len(ps))
	for _, p := range ps {
		out = append(out, *p)
	}
	return oapi.ListPipelines200JSONResponse{Pipelines: out}, nil
}

// CreatePipeline implements POST /api/v1/pipelines.
func (s *Server) CreatePipeline(ctx context.Context, req oapi.CreatePipelineRequestObject) (oapi.CreatePipelineResponseObject, error) {
	spec := ""
	if req.Body != nil {
		spec = req.Body.Spec
	}
	p, err := pipeline.ParseSpec([]byte(spec))
	if err != nil {
		return nil, errStatus(http.StatusBadRequest, err.Error())
	}
	// Captured at creation because jobs spawn later from the executor's ticker,
	// where no request (and so no actor identity) exists anymore.
	p.Tags = s.inheritOwnershipTags(ctx, p.Tags)
	// Link a delegated pipeline back to its owning orchestrator (the agent-mode
	// analogue of Session.ParentID). An explicit owner on the request wins; else
	// fall back to the caller's actor identity, so an agent-created pipeline
	// back-refs its creator without the persona having to pass anything.
	owner := ""
	if req.Body != nil {
		owner = strings.TrimSpace(req.Body.Owner)
	}
	if owner == "" {
		if caller := s.callerSession(ctx); caller != nil {
			owner = caller.ID
		}
	}
	p.OwnerID = owner
	if err := s.exec.pstore.Create(p); errors.Is(err, pipeline.ErrExists) {
		return nil, errStatus(http.StatusConflict, "pipeline "+p.ID+" already exists")
	} else if err != nil {
		return nil, err
	}
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
	for i := range p.Jobs {
		if p.Jobs[i].Status == pipeline.JobRunning || p.Jobs[i].Status == pipeline.JobNeedsAttention {
			return nil, errStatus(http.StatusConflict, "pipeline has live jobs — cancel it first")
		}
	}
	// Reap each settled job's agent session so deleting never orphans agents.
	for i := range p.Jobs {
		if sid := p.Jobs[i].SessionID; sid != "" {
			_ = s.life.Terminate(ctx, sid)
			_ = s.store.Archive(ctx, sid)
		}
	}
	if err := s.exec.pstore.Delete(pid); err != nil {
		return nil, err
	}
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
func (s *Server) PausePipeline(_ context.Context, req oapi.PausePipelineRequestObject) (oapi.PausePipelineResponseObject, error) {
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
func (s *Server) ResumePipeline(_ context.Context, req oapi.ResumePipelineRequestObject) (oapi.ResumePipelineResponseObject, error) {
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
	pid := req.Pid
	p, err := s.exec.pstore.Get(pid)
	if errors.Is(err, pipeline.ErrNotFound) {
		return nil, errStatus(http.StatusNotFound, "pipeline not found")
	}
	if err != nil {
		return nil, err
	}
	if !p.IsCancelable() {
		return nil, errStatus(http.StatusConflict, "pipeline already finished (status "+string(p.Status)+") — nothing to cancel; delete it instead")
	}
	for i := range p.Jobs {
		j := &p.Jobs[i]
		if (j.Status == pipeline.JobRunning || j.Status == pipeline.JobNeedsAttention) && j.SessionID != "" {
			_ = s.life.Terminate(ctx, j.SessionID)
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
		return nil, err
	}
	s.notify()
	s.recordAuditCtx(ctx, audit.ActionPipelineCancel, pid, map[string]string{"name": p.Name})
	return oapi.CancelPipeline200JSONResponse{OKJSONResponse: oapi.OKJSONResponse{Status: "canceled"}}, nil
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

// DonePipelineJob implements POST /api/v1/pipelines/{pid}/jobs/{job}/done — the
// A1 done-signal: a worker's self-reported completion (status + summary) closes
// the job in one shot, no interrogation turn.
func (s *Server) DonePipelineJob(_ context.Context, req oapi.DonePipelineJobRequestObject) (oapi.DonePipelineJobResponseObject, error) {
	var status, summary string
	if req.Body != nil {
		status = req.Body.Status
		summary = req.Body.Summary
	}
	// Background context: done can trigger a reconcile that spawns dependents.
	switch err := s.exec.Done(context.Background(), req.Pid, req.Job, status, summary); {
	case errors.Is(err, pipeline.ErrNotFound):
		return nil, errStatus(http.StatusNotFound, "pipeline not found")
	case errors.Is(err, ErrJobNotFound):
		return nil, errStatus(http.StatusNotFound, "job not found")
	case errors.Is(err, ErrJobNotRunning):
		return nil, errStatus(http.StatusConflict, err.Error())
	case err != nil:
		return nil, err
	}
	return oapi.DonePipelineJob200JSONResponse{OKJSONResponse: oapi.OKJSONResponse{Status: "done"}}, nil
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
