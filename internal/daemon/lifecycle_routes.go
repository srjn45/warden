package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/store"
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
	r.Get("/pressure", s.handlePressure)
	r.Get("/worktrees", s.handleListWorktrees)
	r.Post("/prune", s.handlePrune)
}

// validateSpawnRequest applies the static + uniqueness preconditions for a
// decoded SpawnRequest, returning an HTTP status + message to write on rejection
// or (0, "") when the request is acceptable. It runs the same checks, in the
// same order, that handleSpawn previously inlined — extracted so the handler
// reads as decode → validate → gate → spawn. The memory-pressure soft gate and
// the spawn itself stay in the handler (they have non-error response paths).
func (s *Server) validateSpawnRequest(ctx context.Context, req SpawnRequest) (int, string) {
	// A ticket becomes the session id, which is used as a filesystem path
	// component (the prompt file) and a tmux session name inside Spawn — which
	// runs before store.Insert (the only other safeID gate). Validate up front so
	// an unsafe ticket can't escape the prompts dir or break tmux targeting.
	if req.Ticket != "" {
		if err := store.SafeID(req.Ticket); err != nil {
			return http.StatusBadRequest, "invalid ticket id (no '/', '\\', ':', or '..')"
		}
	}
	// Reject an unknown permission mode up front. req.PermissionMode is
	// concatenated into the claude launch line that Spawn types into a tmux pane;
	// validating here (empty = use the configured default) keeps an unexpected
	// value from reaching the shell and gives a clean 400 instead of a cryptic
	// claude error. The model field is allowed to be any full ID and is
	// shell-quoted at the launch seam (lifecycle.claudeBase) instead.
	if !lifecycle.ValidPermissionMode(req.PermissionMode) {
		return http.StatusBadRequest, "invalid permission mode " + req.PermissionMode +
			"; valid: acceptEdits, auto, bypassPermissions, default, dontAsk, plan"
	}
	// Validate the optional name field for format and uniqueness.
	if req.Name != "" {
		if err := store.ValidateName(req.Name); err != nil {
			return http.StatusBadRequest, err.Error()
		}
		// Check uniqueness: no other session should have the same name.
		sessions, err := s.store.List(ctx)
		if err != nil {
			return http.StatusInternalServerError, "failed to check name uniqueness: " + err.Error()
		}
		for _, sess := range sessions {
			if sess.Name == req.Name {
				return http.StatusConflict, "name already in use: " + req.Name
			}
		}
	}
	freeMode := req.Type == ""
	if !freeMode {
		if req.Repo == "" {
			return http.StatusBadRequest, "typed spawn requires repo"
		}
		// Reject an unknown type rather than silently collapsing it to "other".
		if !store.Type(req.Type).Valid() {
			return http.StatusBadRequest, "unknown type " + req.Type +
				"; valid: development, analysis, spike, pr-review, code, docs, website, debug-ci, tests, other"
		}
	}
	// Reject duplicate spawn on an existing ticket. No-ticket sessions get a
	// random id, so there is nothing to collide on.
	if req.Ticket != "" {
		if _, err := s.store.Get(ctx, req.Ticket); err == nil {
			return http.StatusConflict, "session already exists — use `warden attach " + req.Ticket + "`"
		}
	}
	// Free-form agents launch in the caller's cwd (the "master shell" dir),
	// which is already trusted by Claude Code. It is required — we no longer
	// create a per-agent directory to fall back to — and must be a real dir.
	if freeMode && req.Cwd == "" {
		return http.StatusBadRequest, "provide a launch dir (cwd; prompt optional), or type and repo"
	}
	if req.Cwd != "" {
		if fi, err := os.Stat(req.Cwd); err != nil || !fi.IsDir() {
			return http.StatusBadRequest, "cwd is not an existing directory: " + req.Cwd
		}
	}
	return 0, ""
}

func (s *Server) handleSpawn(w http.ResponseWriter, r *http.Request) {
	var req SpawnRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if code, msg := s.validateSpawnRequest(r.Context(), req); code != 0 {
		writeErr(w, code, msg)
		return
	}
	freeMode := req.Type == ""
	// Memory-pressure soft gate: when enabled and the caller hasn't forced,
	// warn (HTTP 428) instead of spawning onto a strained machine. The client
	// re-spawns with force=true to confirm. Pipelines bypass this (they spawn
	// in-process via SpawnJob, not through this handler).
	s.pressMu.RLock()
	gateOn := s.spawnGate
	s.pressMu.RUnlock()
	if gateOn && !req.Force {
		if v := s.spawnVerdict(r.Context()); v.Elevated {
			writeJSON(w, http.StatusPreconditionRequired, confirmationResponse{
				ConfirmationRequired: true,
				Verdict:              v,
			})
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
		// them — roll back so they don't leak beyond reach of `warden done`.
		tctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if terr := s.life.Teardown(tctx, sess); terr != nil {
			slog.Warn("spawn rollback failed", "agent", sess.ID, "err", terr)
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.notify()
	writeJSON(w, http.StatusCreated, sess)
	if freeMode && req.Prompt != "" {
		go s.classifyAndUpdate(sess.ID, req.Prompt)
	}
}

// handleAdopt registers a Claude session warden did not spawn. It resolves the
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
				slog.Warn("adopt rollback failed", "agent", sess.ID, "err", terr)
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
		slog.Warn("classify update failed", "agent", id, "err", err)
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
	// Terminate sets the session done directly (no poller swap, no event), so
	// reconcile the owning pipeline job here too — otherwise it stays stuck running.
	s.reconcileJobOnTerminal(sess, store.StatusDone)
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
	// Only a hard delete (destroy the record) drops the agent's inbox; an archive
	// keeps it for the historical message-traffic view. Safe to do unconditionally
	// here because no pipeline reads another agent's inbox to make progress —
	// job→job handoff goes through shared context, not the mailbox. Best-effort.
	if req.Hard && s.mbox != nil {
		_ = s.mbox.DeleteInbox(id)
	}
	// Retention policy (worktree_keep_done=false): after a successful archive of a
	// session that owns a worktree, attempt a guarded removal. force=false keeps
	// the dirty/unpushed/agent-alive guard, so a clean worktree is reclaimed while
	// a dirty one is left in place. This is best-effort: it MUST NOT fail the
	// archive (which already succeeded above), so a guard refusal is logged only.
	// Only the archive path applies — a hard delete leaves a record-less orphan
	// that `warden prune` reclaims instead.
	if !req.Hard && s.removeDoneWorktree && sess.Worktree != "" {
		s.removeDoneWorktreeBestEffort(sess)
	}
	s.notify()
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted", "warning": warn})
}

// removeDoneWorktreeBestEffort runs a guarded RemoveWorktree for a just-archived
// worktree-owning session (worktree_keep_done=false). It uses a detached context
// (the originating request is already responding) and force=false so the
// dirty/unpushed/agent-alive guard still protects work-in-progress. A guard
// refusal or any other error is logged and swallowed — the archive already
// succeeded and must not be undone. BranchCreated provenance still gates branch
// deletion (no deleteAdoptedBranch override here).
func (s *Server) removeDoneWorktreeBestEffort(sess *store.Session) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.life.RemoveWorktree(ctx, sess, false, false); err != nil {
		slog.Warn("worktree_keep_done=false: kept worktree on archive", "agent", sess.ID, "err", err)
		return
	}
	slog.Info("worktree_keep_done=false: removed worktree on archive", "agent", sess.ID)
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
	if err := s.life.RemoveWorktree(r.Context(), sess, req.Force, req.DeleteAdoptedBranch); err != nil {
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

// handleListWorktrees serves GET /worktrees?repo=<path>: the read-only join of
// git's .worktrees inventory against active + archived records.
func (s *Server) handleListWorktrees(w http.ResponseWriter, r *http.Request) {
	repo := r.URL.Query().Get("repo")
	if repo == "" {
		writeErr(w, http.StatusBadRequest, "repo is required")
		return
	}
	active, err := s.store.List(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	archived, err := s.store.ListClosed(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	rows, err := s.life.ListWorktrees(r.Context(), repo, active, archived)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if rows == nil {
		rows = []lifecycle.WorktreeListing{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"worktrees": rows})
}

// handlePrune serves POST /prune. Dirty/unpushed worktrees come back as skipped
// entries in the result, not HTTP errors; a missing repo or git failure is an
// HTTP error. Records are read from the store here (the daemon owns the lock)
// and passed into the guard-bearing lifecycle sweep.
func (s *Server) handlePrune(w http.ResponseWriter, r *http.Request) {
	var req pruneRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Repo == "" {
		writeErr(w, http.StatusBadRequest, "repo is required")
		return
	}
	active, err := s.store.List(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	archived, err := s.store.ListClosed(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	results, err := s.life.PruneWorktrees(r.Context(), req.Repo, lifecycle.PruneOpts{
		DryRun:          req.DryRun,
		Force:           req.Force,
		IncludeArchived: req.IncludeArchived,
		Active:          active,
		Archived:        archived,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if results == nil {
		results = []lifecycle.PruneResult{}
	}
	if !req.DryRun {
		s.notify()
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
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
	// The session record exists (we 404'd above otherwise), so a capture
	// failure here means the tmux pane isn't capturable: the agent is still
	// spawning or was just terminated. This endpoint is a best-effort snapshot
	// (the live grid polls it per tile), so degrade to an empty 200 rather than
	// a 500 that floods the browser console during those transient windows.
	out, err := s.life.Output(r.Context(), sess.TmuxSession, lines)
	if err != nil {
		out = ""
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

// SetAutoApproveRequest is the body for PATCH /sessions/{id}/auto-approve.
type SetAutoApproveRequest struct {
	Enabled bool `json:"enabled"`
}

// handleSetAutoApprove updates a session's AutoApprove flag.
func (s *Server) handleSetAutoApprove(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req SetAutoApproveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}

	if err := s.store.UpdateAutoApprove(r.Context(), id, req.Enabled); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "session not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.notify()
	writeJSON(w, http.StatusOK, map[string]bool{"auto_approve": req.Enabled})
}

// SetPermissionModeRequest is the body for PATCH /sessions/{id}/permission-mode.
type SetPermissionModeRequest struct {
	PermissionMode string `json:"permission_mode"`
}

// handleSetPermissionMode updates a session's permission mode.
func (s *Server) handleSetPermissionMode(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req SetPermissionModeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}

	// Validate mode (empty string is allowed - it means use global default).
	if !lifecycle.ValidPermissionMode(req.PermissionMode) {
		writeErr(w, http.StatusBadRequest, "invalid permission mode")
		return
	}

	if err := s.store.UpdatePermissionMode(r.Context(), id, req.PermissionMode); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "session not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.notify()
	writeJSON(w, http.StatusOK, map[string]string{"permission_mode": req.PermissionMode})
}
