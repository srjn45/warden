package daemon

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/store"
)

// spawnAuditDetail captures the who/what context worth keeping for a spawn:
// the agent's name, repo, and task type (omitting any that are empty).
func spawnAuditDetail(sess *store.Session, req SpawnRequest) map[string]string {
	d := map[string]string{}
	if sess.Name != "" {
		d["name"] = sess.Name
	}
	if req.Repo != "" {
		d["repo"] = req.Repo
	}
	if req.Type != "" {
		d["type"] = req.Type
	}
	if len(d) == 0 {
		return nil
	}
	return d
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

// nameAndUpdate runs in the background after a spawn that had no explicit name:
// it derives a human-friendly handle (local LLM when available, else a
// deterministic slug) and assigns it, skipping if no usable handle comes back or
// every candidate collides with an existing agent. Detached context (the request
// is already answered) and best-effort — a missing name never blocks the spawn.
func (s *Server) nameAndUpdate(id, prompt string) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	name := s.life.GenerateName(ctx, prompt)
	if name == "" {
		return
	}
	name = s.uniqueName(ctx, name, id)
	if name == "" {
		return // could not find a free variant; leave the agent unnamed
	}
	if err := s.store.UpdateName(ctx, id, name); err != nil {
		slog.Warn("name update failed", "agent", id, "err", err)
		return
	}
	s.notify()
}

// uniqueName returns name — or a numeric-suffixed variant — that no session other
// than selfID currently uses, or "" when no free variant is found. The store
// rejects duplicate names; this keeps an auto-generated handle from colliding with
// one an operator already chose.
func (s *Server) uniqueName(ctx context.Context, name, selfID string) string {
	sessions, err := s.store.List(ctx)
	if err != nil {
		return name // best-effort: skip the collision check rather than drop the name
	}
	taken := map[string]bool{}
	for _, sess := range sessions {
		if sess.ID != selfID && sess.Name != "" {
			taken[sess.Name] = true
		}
	}
	if !taken[name] {
		return name
	}
	for i := 2; i <= 9; i++ {
		if cand := suffixName(name, i); !taken[cand] {
			return cand
		}
	}
	return ""
}

// suffixName appends "-n" to a generated handle, trimming the base first so the
// result still fits the 32-char stored-name limit. name is ASCII (kebab-case),
// so byte length equals rune length here.
func suffixName(name string, n int) string {
	suffix := "-" + strconv.Itoa(n)
	if len(name)+len(suffix) > 32 {
		name = strings.TrimRight(name[:32-len(suffix)], "-")
	}
	return name + suffix
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
