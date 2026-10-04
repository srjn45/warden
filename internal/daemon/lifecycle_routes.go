package daemon

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/agentname"
	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/store"
)

// spawnAuditDetail captures the who/what context worth keeping for a spawn:
// the agent's name, repo, and task type (omitting any that are empty).
func spawnAuditDetail(sess *agentstore.Agent, req SpawnRequest) map[string]string {
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
	// Explicit names: validate format and uniqueness (409 on collision).
	// Empty names are filled synchronously in prepareSpawnName before Spawn —
	// auto-generated names are disambiguated instead of conflicting.
	if req.Name != "" {
		if err := store.ValidateName(req.Name); err != nil {
			return http.StatusBadRequest, err.Error()
		}
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
	// Managed spawn: explicit Type, a fork (repo resolved adapter-side from
	// fork_from), OR a worktree-owning role (worker) that has a Repo.
	// Role-driven workers no longer rely on a Type default to enter the managed
	// path — see lifecycle.RoleOwnsWorktree. A worker spawned with only Cwd (no
	// Repo) stays free-form, matching the master-shell quick-spawn path.
	managed := req.Type != "" || req.ForkFrom != "" || (lifecycle.RoleOwnsWorktree(req.Role) && req.Repo != "")
	freeMode := !managed
	if !freeMode {
		// A fork's repo is the SOURCE agent's repo, resolved by the lifecycle adapter
		// from fork_from (the caller need not — and `wd fork`/`fork_agent` do not —
		// pass one), so the repo requirement does not apply to a fork.
		if req.Repo == "" && req.ForkFrom == "" {
			return http.StatusBadRequest, "managed spawn requires repo"
		}
		// Reject an unknown type rather than silently collapsing it to "other".
		// Role-only managed spawns leave Type empty (isolation is role-driven).
		if req.Type != "" && !store.Type(req.Type).Valid() {
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
		return http.StatusBadRequest, "provide a launch dir (cwd; prompt optional), or role/type and repo"
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
	if err := s.store.Update(ctx, id, func(sess *agentstore.Agent) error {
		sess.Type = t
		return nil
	}); err != nil {
		slog.Warn("classify update failed", "agent", id, "err", err)
		return
	}
	s.notify()
}

// prepareSpawnName fills req.Name synchronously when the caller left it empty.
// Role conventions (AP:/wkr:/brain:/<pipe>:<stage>), prompt resolution via the
// fast-tier NameRunner, and adjective-noun codenames are tried in order; auto
// names are disambiguated against active sessions so they never 409.
func (s *Server) prepareSpawnName(ctx context.Context, req *SpawnRequest) {
	if req == nil || strings.TrimSpace(req.Name) != "" {
		return
	}
	req.Name = agentname.ResolveSpawnName(ctx, spawnNameInput(*req), s.nameRunner(), s.existingAgentNames(ctx))
}

func (s *Server) existingAgentNames(ctx context.Context) map[string]bool {
	taken := map[string]bool{}
	if s.store == nil {
		return taken
	}
	sessions, err := s.store.List(ctx)
	if err != nil {
		return taken
	}
	for _, sess := range sessions {
		if sess.Name != "" {
			taken[sess.Name] = true
		}
	}
	return taken
}

func (s *Server) nameRunner() agentname.BackendRunner {
	if s.promptNamer != nil {
		return s.promptNamer
	}
	if s.life == nil {
		return nil
	}
	// Prefer a lifecycle-backed runner when the adapter exposes one; otherwise
	// ResolvePromptName falls back to GenerateCodename (never blocks spawn).
	if r, ok := s.life.(interface {
		NameRunner() agentname.BackendRunner
	}); ok {
		return r.NameRunner()
	}
	return nil
}

func spawnNameInput(req SpawnRequest) agentname.SpawnNameInput {
	planSlug := ""
	ticket := strings.TrimSpace(req.Ticket)
	if strings.HasSuffix(ticket, "-autopilot") {
		planSlug = strings.TrimSuffix(ticket, "-autopilot")
	}
	if planSlug == "" {
		planSlug = strings.TrimSpace(req.PlanID)
	}
	return agentname.SpawnNameInput{
		Explicit: req.Name,
		Role:     req.Role,
		Prompt:   req.Prompt,
		PlanSlug: planSlug,
		// AutopilotTaskID only — Task is the tier-routing registry name.
		TaskID:   req.AutopilotTaskID,
		TargetID: firstNonEmpty(req.ParentID, req.Ticket),
	}
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
func (s *Server) removeDoneWorktreeBestEffort(sess *agentstore.Agent) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.life.RemoveWorktree(ctx, sess, false, false); err != nil {
		slog.Warn("worktree_keep_done=false: kept worktree on archive", "agent", sess.ID, "err", err)
		return
	}
	s.recordPlanBoundWorktreeRemoved(sess)
	slog.Info("worktree_keep_done=false: removed worktree on archive", "agent", sess.ID)
}
