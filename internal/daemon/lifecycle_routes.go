package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/srajanpathak/agentctl/internal/lifecycle"
	"github.com/srajanpathak/agentctl/internal/store"
)

func (s *Server) registerLifecycleRoutes(r chi.Router) {
	r.Post("/spawn", s.handleSpawn)
	r.Post("/cleanup", s.handleCleanup)
	r.Post("/sessions/{id}/input", s.handleInput)
	r.Get("/sessions/{id}/output", s.handleOutput)
}

func (s *Server) handleSpawn(w http.ResponseWriter, r *http.Request) {
	var req SpawnRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	promptMode := req.Prompt != "" && req.Type == ""
	if !promptMode {
		if req.Type == "" || req.Repo == "" {
			writeErr(w, http.StatusBadRequest, "provide a prompt, or type and repo")
			return
		}
		// Reject an unknown type rather than silently collapsing it to "other".
		if !store.Type(req.Type).Valid() {
			writeErr(w, http.StatusBadRequest, "unknown type "+req.Type+
				"; valid: development, analysis, spike, pr-review, buildkite-debug, test-run, env-test, other")
			return
		}
	}
	// Reject duplicate spawn on an existing ticket. No-ticket sessions get a
	// random id, so there is nothing to collide on.
	if req.Ticket != "" {
		if _, err := s.store.Get(r.Context(), req.Ticket); err == nil {
			writeErr(w, http.StatusConflict, "session already exists — use `agentctl attach "+req.Ticket+"`")
			return
		}
	}
	if promptMode {
		req.Workdir = s.workdir
	}
	sess, err := s.life.Spawn(r.Context(), req)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.store.Insert(r.Context(), sess); err != nil {
		// The tmux session (and any worktree) already exist but no doc tracks
		// them — roll back so they don't leak beyond reach of `agentctl done`.
		tctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if terr := s.life.Teardown(tctx, sess); terr != nil {
			log.Printf("spawn rollback %s: %v", sess.ID, terr)
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.notify()
	writeJSON(w, http.StatusCreated, sess)
	if promptMode {
		go s.classifyAndUpdate(sess.ID, req.Prompt)
	}
}

// classifyAndUpdate runs in the background after a prompt-spawn: it labels the
// agent's type via the LLM and updates the doc. Uses a detached context because
// the request context is already done by the time this runs.
func (s *Server) classifyAndUpdate(id, prompt string) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	t, err := s.life.Classify(ctx, prompt)
	if err != nil {
		t = store.TypeOther // never block: fall back to "other"
	}
	if err := s.store.UpdateType(ctx, id, t); err != nil {
		log.Printf("classify update %s: %v", id, err)
		return
	}
	s.notify()
}

func (s *Server) handleCleanup(w http.ResponseWriter, r *http.Request) {
	var req CleanupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == "" {
		writeErr(w, http.StatusBadRequest, "id is required")
		return
	}
	ctx := r.Context()
	if err := s.life.Cleanup(ctx, req.ID, req.Force, req.Hard); err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeErr(w, http.StatusNotFound, "session not found")
		case errors.Is(err, lifecycle.ErrDirtyWorktree), errors.Is(err, lifecycle.ErrUnpushedCommits):
			// The guard deliberately preserves uncommitted/unpushed work; a 409
			// tells the client to push (or retry with --force).
			writeErr(w, http.StatusConflict, err.Error())
		default:
			// tmux kill / git worktree-remove failure — a server-side fault.
			writeErr(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	// The tmux session and worktree are gone; the record must now reflect that.
	// If this write fails the doc would dangle pointing at dead resources, so we
	// surface it rather than reporting a clean success. A doc that is already
	// absent (ErrNotFound) is the desired end state, so it is treated as success.
	var derr error
	if req.Hard {
		derr = s.store.Delete(ctx, req.ID)
	} else {
		derr = s.store.Archive(ctx, req.ID)
	}
	if derr != nil && !errors.Is(derr, store.ErrNotFound) {
		writeErr(w, http.StatusInternalServerError, "resources removed but session record not updated: "+derr.Error())
		return
	}
	s.notify()
	writeJSON(w, http.StatusOK, map[string]string{"status": "cleaned"})
}

func (s *Server) handleInput(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	sess, err := s.store.Get(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	var req InputRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if err := s.life.Input(r.Context(), sess.TmuxSession, req.Text); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.notify()
	writeJSON(w, http.StatusOK, map[string]string{"status": "sent"})
}

func (s *Server) handleOutput(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	sess, err := s.store.Get(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	lines := 200
	if q := r.URL.Query().Get("lines"); q != "" {
		if n, err := strconv.Atoi(q); err == nil {
			lines = n
		}
	}
	out, err := s.life.Output(r.Context(), sess.TmuxSession, lines)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, OutputResponse{Output: out})
}
