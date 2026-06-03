package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/srajanpathak/agentctl/internal/lifecycle"
	"github.com/srajanpathak/agentctl/internal/store"
)

func (s *Server) registerLifecycleRoutes(r chi.Router) {
	r.Post("/spawn", s.handleSpawn)
	r.Post("/adopt", s.handleAdopt)
	r.Post("/sessions/{id}/terminate", s.handleTerminate)
	r.Post("/sessions/{id}/delete", s.handleDelete)
	r.Post("/sessions/{id}/remove-worktree", s.handleRemoveWorktree)
	r.Post("/sessions/{id}/input", s.handleInput)
	r.Get("/sessions/{id}/output", s.handleOutput)
	r.Get("/sessions/{id}/attach", s.handleAttach)
	r.Post("/sessions/{id}/restore", s.handleRestore)
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
	// Prompt-mode agents launch in the caller's cwd (the "master shell" dir),
	// which is already trusted by Claude Code. It is required — we no longer
	// create a per-agent directory to fall back to — and must be a real dir.
	if promptMode && req.Cwd == "" {
		writeErr(w, http.StatusBadRequest, "prompt-mode spawn requires cwd (the directory to launch the agent in)")
		return
	}
	if req.Cwd != "" {
		if fi, err := os.Stat(req.Cwd); err != nil || !fi.IsDir() {
			writeErr(w, http.StatusBadRequest, "cwd is not an existing directory: "+req.Cwd)
			return
		}
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

// handleAdopt registers a Claude session agentctl did not spawn. It resolves the
// claude session id (explicit override, else newest transcript for cwd), refuses
// to adopt a conversation an active session already tracks, then delegates to
// Lifecycle.Adopt (resume-under-tmux when tmux_session is empty, live register
// otherwise) and persists the record. Rollback (kill tmux) runs ONLY in resume
// mode — a live adoption never owns the tmux session, so it must never kill it.
func (s *Server) handleAdopt(w http.ResponseWriter, r *http.Request) {
	var req AdoptRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if req.Cwd == "" {
		writeErr(w, http.StatusBadRequest, "adopt requires cwd")
		return
	}
	if fi, err := os.Stat(req.Cwd); err != nil || !fi.IsDir() {
		writeErr(w, http.StatusBadRequest, "cwd is not an existing directory: "+req.Cwd)
		return
	}

	resume := req.TmuxSession == ""

	// Resolve the claude session id: explicit override, else newest for cwd.
	claudeID := req.SessionID
	if claudeID == "" {
		if id, err := s.life.NewestClaudeSession(r.Context(), req.Cwd); err == nil {
			claudeID = id
		}
	}
	if resume && claudeID == "" {
		writeErr(w, http.StatusBadRequest, "no claude session found to resume in "+req.Cwd+" (pass session_id)")
		return
	}

	// Two-heads guard: never adopt a conversation an active session already tracks.
	if claudeID != "" {
		sessions, err := s.store.List(r.Context())
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		for _, ex := range sessions {
			if ex.ClaudeSessionID == claudeID {
				writeErr(w, http.StatusConflict, "claude session already adopted as "+ex.ID)
				return
			}
		}
	}

	// Choose the agent id. Live mode keeps the existing tmux name when it is a
	// safe, unused id; otherwise leave it empty so Lifecycle generates one (and
	// renames the tmux session to match).
	chosenID := ""
	if !resume && store.SafeID(req.TmuxSession) == nil {
		if _, err := s.store.Get(r.Context(), req.TmuxSession); errors.Is(err, store.ErrNotFound) {
			chosenID = req.TmuxSession
		}
	}

	sess, err := s.life.Adopt(r.Context(), AdoptParams{
		ID: chosenID, Cwd: req.Cwd, ClaudeSessionID: claudeID, TmuxSession: req.TmuxSession,
	})
	if err != nil {
		if errors.Is(err, lifecycle.ErrTmuxGone) {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	if err := s.store.Insert(r.Context(), sess); err != nil {
		// Only resume mode created the tmux session; never kill a live one.
		// Roll back on ANY insert failure (including ErrExists) so a resume-mode
		// tmux session never leaks.
		if resume {
			tctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if terr := s.life.Teardown(tctx, sess); terr != nil {
				log.Printf("adopt rollback %s: %v", sess.ID, terr)
			}
		}
		if errors.Is(err, store.ErrExists) {
			writeErr(w, http.StatusConflict, "already registered: "+sess.ID)
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	warn := ""
	if claudeID == "" {
		warn = "registered without a claude session id (monitoring only; restore unavailable)"
	}
	s.notify()
	writeJSON(w, http.StatusCreated, adoptResponse{Session: sess, Warning: warn})
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

// liveStatus reports whether the stored status implies the agent may still be
// running (so delete can warn instead of silently orphaning a live tmux).
func liveStatus(s store.Status) bool {
	switch s {
	case store.StatusSpawning, store.StatusWorking, store.StatusWaitingForInput, store.StatusIdle:
		return true
	}
	return false
}

func (s *Server) handleTerminate(w http.ResponseWriter, r *http.Request) {
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
	if err := s.life.Terminate(r.Context(), sess.TmuxSession); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.store.UpdateStatus(r.Context(), id, store.StatusDone); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.notify()
	writeJSON(w, http.StatusOK, map[string]string{"status": "terminated"})
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req deleteRequest
	_ = json.NewDecoder(r.Body).Decode(&req) // empty body ok → archive
	sess, err := s.store.Get(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	warn := ""
	if liveStatus(sess.Status) {
		warn = "agent may still be running (status " + string(sess.Status) + "); terminate it first or it becomes untracked"
	}
	var derr error
	if req.Hard {
		derr = s.store.Delete(r.Context(), id)
	} else {
		derr = s.store.Archive(r.Context(), id)
	}
	if derr != nil && !errors.Is(derr, store.ErrNotFound) {
		writeErr(w, http.StatusInternalServerError, derr.Error())
		return
	}
	s.notify()
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted", "warning": warn})
}

func (s *Server) handleRemoveWorktree(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req removeWorktreeRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	sess, err := s.store.Get(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.life.RemoveWorktree(r.Context(), sess, req.Force); err != nil {
		switch {
		case errors.Is(err, lifecycle.ErrNoWorktree):
			writeErr(w, http.StatusUnprocessableEntity, err.Error())
		case errors.Is(err, lifecycle.ErrWorktreeAgentAlive),
			errors.Is(err, lifecycle.ErrDirtyWorktree),
			errors.Is(err, lifecycle.ErrUnpushedCommits):
			writeErr(w, http.StatusConflict, err.Error())
		default:
			writeErr(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	if err := s.store.ClearWorktree(r.Context(), id); err != nil && !errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.notify()
	writeJSON(w, http.StatusOK, map[string]string{"status": "worktree removed"})
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

func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request) {
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
	if err := s.life.Restore(r.Context(), sess); err != nil {
		switch {
		case errors.Is(err, lifecycle.ErrAlreadyRunning):
			writeErr(w, http.StatusConflict, err.Error())
		case errors.Is(err, lifecycle.ErrNoSessionID),
			errors.Is(err, lifecycle.ErrWorkdirMissing),
			errors.Is(err, lifecycle.ErrNoTranscript):
			writeErr(w, http.StatusUnprocessableEntity, err.Error())
		default:
			writeErr(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	if err := s.store.UpdateStatus(r.Context(), id, store.StatusSpawning); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.notify()
	writeJSON(w, http.StatusOK, map[string]string{"status": "restoring"})
}
