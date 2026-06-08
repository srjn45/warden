package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/srajanpathak/warden/internal/pipeline"
)

type createPipelineRequest struct {
	Spec string `json:"spec"` // raw YAML
}
type pipelinesResponse struct {
	Pipelines []*pipeline.Pipeline `json:"pipelines"`
}
type emitRequest struct {
	Text string `json:"text"`
}
type editJobRequest struct {
	Prompt  *string `json:"prompt,omitempty"`
	Handoff *string `json:"handoff,omitempty"`
}

func (s *Server) registerPipelineRoutes(r chi.Router) {
	r.Post("/pipelines", s.handleCreatePipeline)
	r.Get("/pipelines", s.handleListPipelines)
	r.Get("/pipelines/{pid}", s.handleShowPipeline)
	r.Delete("/pipelines/{pid}", s.handleDeletePipeline)
	r.Post("/pipelines/{pid}/start", s.handleStartPipeline)
	r.Post("/pipelines/{pid}/cancel", s.handleCancelPipeline)
	r.Post("/pipelines/{pid}/jobs/{job}/emit", s.handleEmit)
	r.Post("/pipelines/{pid}/jobs/{job}/edit", s.handleEditJob)
	r.Post("/pipelines/{pid}/jobs/{job}/retry", s.handleRetry)
}

func (s *Server) handleCreatePipeline(w http.ResponseWriter, r *http.Request) {
	var req createPipelineRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	p, err := pipeline.ParseSpec([]byte(req.Spec))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.exec.pstore.Create(p); errors.Is(err, pipeline.ErrExists) {
		writeErr(w, http.StatusConflict, "pipeline "+p.ID+" already exists")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (s *Server) handleListPipelines(w http.ResponseWriter, r *http.Request) {
	ps, err := s.exec.pstore.List()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, pipelinesResponse{Pipelines: ps})
}

func (s *Server) handleShowPipeline(w http.ResponseWriter, r *http.Request) {
	p, err := s.exec.pstore.Get(chi.URLParam(r, "pid"))
	if errors.Is(err, pipeline.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "pipeline not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// handleDeletePipeline removes a pipeline's record. It refuses while any job is
// still live (running / needs_attention) so live agents are never orphaned —
// cancel the pipeline first.
func (s *Server) handleDeletePipeline(w http.ResponseWriter, r *http.Request) {
	pid := chi.URLParam(r, "pid")
	p, err := s.exec.pstore.Get(pid)
	if errors.Is(err, pipeline.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "pipeline not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	for i := range p.Jobs {
		if p.Jobs[i].Status == pipeline.JobRunning || p.Jobs[i].Status == pipeline.JobNeedsAttention {
			writeErr(w, http.StatusConflict, "pipeline has live jobs — cancel it first")
			return
		}
	}
	// Reap each job's agent session so deleting the pipeline never orphans agents
	// (a finished/failed job's tmux can still be alive). Best-effort: terminate the
	// tmux+claude process and archive the record (keeps the worktree, mirroring the
	// per-agent delete default). The live-job guard above already excluded running /
	// needs_attention jobs, so this only ever reaps settled sessions.
	for i := range p.Jobs {
		if sid := p.Jobs[i].SessionID; sid != "" {
			_ = s.life.Terminate(r.Context(), sid)
			_ = s.store.Archive(r.Context(), sid)
		}
	}
	if err := s.exec.pstore.Delete(pid); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.notify()
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleStartPipeline(w http.ResponseWriter, r *http.Request) {
	pid := chi.URLParam(r, "pid")
	p, err := s.exec.pstore.Get(pid)
	if errors.Is(err, pipeline.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "pipeline not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if p.Status != pipeline.StatusPending {
		writeErr(w, http.StatusConflict, "pipeline already started (status "+string(p.Status)+")")
		return
	}
	if err := s.exec.pstore.Update(pid, func(p *pipeline.Pipeline) { p.Status = pipeline.StatusRunning }); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Reconcile on a daemon-owned context: spawning worktree jobs can outlast the
	// triggering request, and DAG progress must not be tied to it (spec §15).
	if err := s.exec.Reconcile(context.Background(), pid); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "started"})
}

func (s *Server) handleCancelPipeline(w http.ResponseWriter, r *http.Request) {
	pid := chi.URLParam(r, "pid")
	p, err := s.exec.pstore.Get(pid)
	if errors.Is(err, pipeline.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "pipeline not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Terminate any live job sessions (best-effort), then mark canceled. A
	// needs_attention job's tmux session is typically still alive, so terminate
	// it too — not just running jobs.
	for i := range p.Jobs {
		j := &p.Jobs[i]
		if (j.Status == pipeline.JobRunning || j.Status == pipeline.JobNeedsAttention) && j.SessionID != "" {
			_ = s.life.Terminate(r.Context(), j.SessionID)
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
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.notify()
	writeJSON(w, http.StatusOK, map[string]string{"status": "canceled"})
}

func (s *Server) handleEmit(w http.ResponseWriter, r *http.Request) {
	pid, job := chi.URLParam(r, "pid"), chi.URLParam(r, "job")
	var req emitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if req.Text == "" {
		writeErr(w, http.StatusBadRequest, "empty emit text")
		return
	}
	// Background context: emit can trigger a reconcile that spawns dependents,
	// which may outlast the agent's request.
	err := s.exec.Emit(context.Background(), pid, job, req.Text)
	switch {
	case errors.Is(err, pipeline.ErrNotFound):
		writeErr(w, http.StatusNotFound, "pipeline not found")
	case errors.Is(err, ErrJobNotFound):
		writeErr(w, http.StatusNotFound, "job not found")
	case errors.Is(err, ErrJobNotRunning):
		writeErr(w, http.StatusConflict, err.Error())
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err.Error())
	default:
		writeJSON(w, http.StatusOK, map[string]string{"status": "emitted"})
	}
}

func (s *Server) handleEditJob(w http.ResponseWriter, r *http.Request) {
	pid, job := chi.URLParam(r, "pid"), chi.URLParam(r, "job")
	var req editJobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if req.Prompt == nil && req.Handoff == nil {
		writeErr(w, http.StatusBadRequest, "nothing to edit (provide prompt and/or handoff)")
		return
	}
	err := s.exec.EditJob(pid, job, req.Prompt, req.Handoff)
	switch {
	case errors.Is(err, pipeline.ErrNotFound):
		writeErr(w, http.StatusNotFound, "pipeline not found")
	case errors.Is(err, ErrJobNotFound):
		writeErr(w, http.StatusNotFound, "job not found")
	case errors.Is(err, ErrJobNotPending):
		writeErr(w, http.StatusConflict, "job is not pending (can only edit before it starts)")
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err.Error())
	default:
		writeJSON(w, http.StatusOK, map[string]string{"status": "edited"})
	}
}

func (s *Server) handleRetry(w http.ResponseWriter, r *http.Request) {
	pid, job := chi.URLParam(r, "pid"), chi.URLParam(r, "job")
	// Background context: retry reconciles and may spawn worktree jobs.
	err := s.exec.Retry(context.Background(), pid, job)
	switch {
	case errors.Is(err, pipeline.ErrNotFound):
		writeErr(w, http.StatusNotFound, "pipeline not found")
	case errors.Is(err, ErrJobNotFound):
		writeErr(w, http.StatusNotFound, "job not found")
	case errors.Is(err, ErrJobNotRetryable):
		writeErr(w, http.StatusConflict, "job is not failed or needs-attention")
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err.Error())
	default:
		writeJSON(w, http.StatusOK, map[string]string{"status": "retrying"})
	}
}
