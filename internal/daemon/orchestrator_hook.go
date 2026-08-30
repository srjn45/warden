package daemon

import (
	"context"
	"log/slog"
	"strings"

	"github.com/srjn45/warden/internal/projectstore"
	"github.com/srjn45/warden/internal/store"
)

// orchestratorRole is the built-in role the auto-spawned per-project orchestrator
// runs under (internal/role/roles/orchestrator.yaml): it coordinates the project's
// fleet rather than writing code itself.
const orchestratorRole = "orchestrator"

// orchAgentName is the deterministic name of the orchestrator warden guarantees for
// a project: "orch-" + a sanitized, length-capped slug of the project's display
// name. Because the name is derived only from the project it is stable across
// reopens, so the "is one already alive?" check below can match on the name alone.
func orchAgentName(projectName string) string {
	slug := orchSlug(projectName)
	if slug == "" {
		slug = "project"
	}
	name := "orch-" + slug
	// store.ValidateName caps names at 32 chars; trim to fit while keeping the
	// "orch-" prefix and never leaving a trailing hyphen.
	if len(name) > 32 {
		name = strings.Trim(name[:32], "-")
	}
	return name
}

// orchSlug lower-cases s and keeps only [a-z0-9-], collapsing any run of other
// characters (spaces, punctuation) into a single hyphen and trimming edge hyphens,
// so the result is always a valid store name fragment (mirrors lifecycle.parseName).
func orchSlug(s string) string {
	var b strings.Builder
	prevHyphen := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevHyphen = false
		default:
			if b.Len() > 0 && !prevHyphen {
				b.WriteByte('-')
				prevHyphen = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// guaranteeOrchestrator ensures project p has exactly one live orchestrator agent
// named orch-<project> running in the project root (Project Groups feature, Phase 2:
// the daemon-side auto-spawn hook fired when a project is opened/activated in the
// Cockpit). It is idempotent:
//
//   - a live agent with that name anywhere ⇒ no-op (never spawn a second — the name
//     is globally unique, so a duplicate spawn would 409 regardless);
//   - a recorded-but-dead agent with that name in THIS project ⇒ revive it from its
//     transcript rather than mint a fresh one (mirrors restoreHibernatedAgents);
//   - none ⇒ spawn a fresh orchestrator (role orchestrator, workdir = project path).
//
// Best-effort: every failure is logged and swallowed so the open request that
// triggered it still succeeds. Callers invoke it after the project has been flipped
// open (and after restoreHibernatedAgents, so a hibernated orch is already live by
// the time we look).
func (s *Server) guaranteeOrchestrator(ctx context.Context, p projectstore.Project) {
	if s.store == nil || s.life == nil {
		return
	}
	// Without a launch dir there is nowhere to run the orchestrator; the record-only
	// open paths (a remote not yet cloned) leave Path empty.
	if p.Path == "" {
		return
	}
	name := orchAgentName(p.Name)
	all, err := s.store.List(ctx)
	if err != nil {
		slog.Warn("daemon: orchestrator: list sessions failed", "project", p.ID, "err", err)
		return
	}
	var revive *store.Session
	for _, sess := range all {
		if sess.Name != name {
			continue
		}
		if liveStatus(sess.Status) {
			return // already alive — idempotent no-op
		}
		if sessionInProject(sess, p) {
			revive = sess // this project's own dead orch: prefer reviving it
		}
	}
	if revive != nil {
		s.reviveOrchestrator(ctx, revive)
		return
	}
	s.spawnOrchestrator(ctx, p, name)
}

// reviveOrchestrator brings a recorded-but-dead orchestrator back to life from its
// transcript, mirroring restoreHibernatedAgents (restore → mark spawning + clear the
// hibernated flag). Best-effort.
func (s *Server) reviveOrchestrator(ctx context.Context, sess *store.Session) {
	if err := s.life.Restore(ctx, sess); err != nil {
		slog.Warn("daemon: orchestrator: revive failed", "agent", sess.ID, "err", err)
		return
	}
	if err := s.store.Update(ctx, sess.ID, func(u *store.Session) error {
		u.Status = store.StatusSpawning
		u.Hibernated = false
		return nil
	}); err != nil {
		slog.Warn("daemon: orchestrator: revive clear-flag failed", "agent", sess.ID, "err", err)
		return
	}
	s.notify()
}

// spawnOrchestrator launches a fresh orchestrator for project p in its root, then
// stamps the ProjectID back-ref so it hibernates/restores with the project. It
// mirrors the spawn → insert → rollback-on-insert-failure flow the HTTP spawn
// handler and autopilot brain use. Best-effort.
func (s *Server) spawnOrchestrator(ctx context.Context, p projectstore.Project, name string) {
	req := SpawnRequest{
		Cwd:     p.Path, // free-form (no type) ⇒ launch in the project root
		Name:    name,
		Role:    orchestratorRole,
		Backend: s.defaultBackend(),
	}
	if code, msg := s.validateSpawnRequest(ctx, req); code != 0 {
		slog.Warn("daemon: orchestrator: spawn rejected", "project", p.ID, "code", code, "msg", msg)
		return
	}
	sess, err := s.life.Spawn(ctx, req)
	if err != nil {
		slog.Warn("daemon: orchestrator: spawn failed", "project", p.ID, "err", err)
		return
	}
	sess.ProjectID = p.ID // explicit back-ref so close/reopen tracks this orch
	if err := s.store.Insert(ctx, sess); err != nil {
		// Roll back the tmux session so a failed insert doesn't leak an untracked
		// orchestrator (same guard the HTTP spawn handler applies).
		tctx, cancel := context.WithTimeout(context.Background(), brainTeardownTimeout)
		defer cancel()
		if terr := s.life.Teardown(tctx, sess); terr != nil {
			slog.Warn("daemon: orchestrator: spawn rollback failed", "agent", sess.ID, "err", terr)
		}
		slog.Warn("daemon: orchestrator: insert failed", "project", p.ID, "err", err)
		return
	}
	s.notify()
	slog.Info("daemon: orchestrator: auto-spawned", "project", p.ID, "agent", sess.ID, "name", name)
}
