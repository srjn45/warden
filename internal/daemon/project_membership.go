package daemon

import (
	"log/slog"

	"github.com/srjn45/warden/internal/projectstore"
	"github.com/srjn45/warden/internal/store"
)

// Project membership stamping on session create/teardown
// (docs/specs/2026-09-25-project-entity-hierarchy.md D2/§3.1, §5, §6). A session
// (agent or terminal) is a member of exactly one project; that membership is
// stored authoritatively on the project as agents[]/terminals[] and mirrored by
// the per-session ProjectID back-ref. These helpers keep the two ends consistent
// at the two lifecycle edges this phase owns: user-facing spawn/adopt (add) and
// delete/teardown (remove). Pipeline membership and agent child lists are wired in
// a later phase; this phase covers only the project↔session edge.

// resolveProjectID returns the id of the project a freshly created (or adopted)
// session belongs to. An explicit request-supplied id (already stamped onto
// sess.ProjectID by lifecycle) always wins; otherwise the session's on-disk launch
// location is path-matched against the OPEN projects (the daemon owns the projects
// store, so this resolution lives here, not in the store-free lifecycle). Returns
// "" when no projects store is wired or no open project matches — the session is
// then project-less and joins no membership list. A closed (hibernated) project is
// never auto-matched: opening it is what re-associates its members.
func (s *Server) resolveProjectID(sess *store.Session) string {
	if sess == nil {
		return ""
	}
	if sess.ProjectID != "" {
		return sess.ProjectID // explicit request param — wins over path-match
	}
	if s.projects == nil {
		return ""
	}
	projs, err := s.projects.List()
	if err != nil {
		slog.Warn("daemon: project membership: list projects failed", "agent", sess.ID, "err", err)
		return ""
	}
	for _, p := range projs {
		if projectstore.NormalizeStatus(p.Status) != projectstore.StatusOpen {
			continue
		}
		// sessionInProject falls back to a path-match when ProjectID is empty (the
		// case here), mapping a worktree checkout to its parent project root.
		if sessionInProject(sess, p) {
			return p.ID
		}
	}
	return ""
}

// stampProjectMembership resolves the owning project for a not-yet-inserted session
// and stamps sess.ProjectID with it, so the back-ref persists in the same store
// write as the insert. It only ever fills an empty id (an explicit request id or a
// prior stamp is left untouched) and is a no-op when no project resolves. Call
// BEFORE store.Insert; pair it with addProjectMembership AFTER a successful insert.
func (s *Server) stampProjectMembership(sess *store.Session) {
	if sess == nil || sess.ProjectID != "" {
		return
	}
	sess.ProjectID = s.resolveProjectID(sess)
}

// addProjectMembership appends an inserted session to its project's authoritative
// membership list (spec D2/§3.1): Project.terminals[] for a terminal, else
// Project.agents[]. Best-effort — the add de-duplicates (a repeat is a no-op) and a
// failure is logged, never fatal: the session keeps its ProjectID back-ref
// regardless, and the two edges are reconciled with the project list as the source
// of truth. A project-less session (empty ProjectID) or an unconfigured projects
// store is a silent no-op. Call AFTER a successful store.Insert.
func (s *Server) addProjectMembership(sess *store.Session) {
	if s.projects == nil || sess == nil || sess.ProjectID == "" {
		return
	}
	var err error
	if sess.IsTerminal() {
		_, err = s.projects.AddTerminalToProject(sess.ProjectID, sess.ID)
	} else {
		_, err = s.projects.AddAgentToProject(sess.ProjectID, sess.ID)
	}
	if err != nil {
		slog.Warn("daemon: project membership: add failed", "agent", sess.ID, "project", sess.ProjectID, "kind", sess.Kind, "err", err)
	}
}

// removeProjectMembership drops a torn-down session from its project's membership
// list, the delete-side mirror of addProjectMembership. Best-effort (removing an
// absent member is a no-op) and a silent no-op for a project-less session or an
// unconfigured store. Call when a session is deleted/archived (not merely
// terminated: a still-recorded orphaned/hibernated member is tolerated as a
// dangling id per spec §6.3, so terminate leaves membership intact).
func (s *Server) removeProjectMembership(sess *store.Session) {
	if s.projects == nil || sess == nil || sess.ProjectID == "" {
		return
	}
	var err error
	if sess.IsTerminal() {
		_, err = s.projects.RemoveTerminalFromProject(sess.ProjectID, sess.ID)
	} else {
		_, err = s.projects.RemoveAgentFromProject(sess.ProjectID, sess.ID)
	}
	if err != nil {
		slog.Warn("daemon: project membership: remove failed", "agent", sess.ID, "project", sess.ProjectID, "kind", sess.Kind, "err", err)
	}
}
