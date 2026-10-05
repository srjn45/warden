package daemon

import (
	"log/slog"
	"path/filepath"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/projectstore"
)

// Project membership stamping on session create/teardown
// (docs/specs/2026-09-25-project-entity-hierarchy.md D2/§3.1, §5, §6). An agent
// is a member of exactly one project; that membership is
// stored authoritatively on the project as agents[] and mirrored by
// the per-agent ProjectID back-ref. These helpers keep the two ends consistent
// at the two lifecycle edges this phase owns: user-facing spawn/adopt (add) and
// delete/teardown (remove).

// resolveProjectID returns the id of the project a freshly created (or adopted)
// agent belongs to. An explicit request-supplied id (already stamped onto
// sess.ProjectID by lifecycle) always wins; otherwise the agent's on-disk launch
// location is path-matched against the OPEN projects (the daemon owns the projects
// store, so this resolution lives here, not in the store-free lifecycle). Returns
// "" when no projects store is wired or no open project matches — the agent is
// then project-less and joins no membership list. When no open project matches, the
// launch directory is auto-registered (or a closed/hibernated project there is
// reopened) via ensureOpenProject, so the agent always lands under a project.
func (s *Server) resolveProjectID(sess *agentstore.Agent) string {
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
	return s.ensureProjectID(projectSourceDir(sess), sess.ID)
}

// ensureOpenProject guarantees an OPEN project exists for dir (zero-touch
// registration). dir is normalized so a .worktrees/<name> checkout maps to its
// parent repo root. A missing project is created open (ID=Path=dir, Name=base);
// a closed one is reopened keeping its name, settings, groups, plans and member
// lists; an already-open one is returned untouched with no write. Returns nil, nil
// when no projects store is wired or dir is empty.
func (s *Server) ensureOpenProject(dir string) (*projectstore.Project, error) {
	if s.projects == nil || dir == "" {
		return nil, nil
	}
	dir = normalizeProjectDir(dir)
	if dir == "" {
		return nil, nil
	}
	projs, err := s.projects.List()
	if err != nil {
		return nil, err
	}
	for _, p := range projs {
		if p.ID != dir && p.Path != dir {
			continue
		}
		if projectstore.NormalizeStatus(p.Status) == projectstore.StatusOpen {
			return &p, nil
		}
		// Empty name/path leave the stored values intact on reopen.
		reopened, err := s.projects.OpenProject(p.ID, "", "")
		if err != nil {
			return nil, err
		}
		return &reopened, nil
	}
	created, err := s.projects.OpenProject(dir, filepath.Base(dir), dir)
	if err != nil {
		return nil, err
	}
	return &created, nil
}

// ensureProjectID is the best-effort id form of ensureOpenProject for the
// resolve* fallbacks: any failure is logged and yields "" (project-less).
func (s *Server) ensureProjectID(dir, owner string) string {
	p, err := s.ensureOpenProject(dir)
	if err != nil {
		slog.Warn("daemon: project auto-register failed", "owner", owner, "dir", dir, "err", err)
		return ""
	}
	if p == nil {
		return ""
	}
	return p.ID
}

// stampProjectMembership resolves the owning project for a not-yet-inserted agent
// and stamps sess.ProjectID with it, so the back-ref persists in the same store
// write as the insert. It only ever fills an empty id (an explicit request id or a
// prior stamp is left untouched) and is a no-op when no project resolves. Call
// BEFORE store.Insert; pair it with addProjectMembership AFTER a successful insert.
func (s *Server) stampProjectMembership(sess *agentstore.Agent) {
	if sess == nil || sess.ProjectID != "" {
		return
	}
	sess.ProjectID = s.resolveProjectID(sess)
}

// addProjectMembership appends an inserted agent to its project's authoritative
// membership list (spec D2/§3.1): Project.agents[]. Best-effort — the add de-duplicates
// (a repeat is a no-op) and a failure is logged, never fatal: the agent keeps its
// ProjectID back-ref regardless, and the two edges are reconciled with the project list
// as the source of truth. A project-less agent (empty ProjectID) or an unconfigured projects
// store is a silent no-op. Call AFTER a successful store.Insert.
func (s *Server) addProjectMembership(sess *agentstore.Agent) {
	if s.projects == nil || sess == nil || sess.ProjectID == "" {
		return
	}
	if _, err := s.projects.AddAgentToProject(sess.ProjectID, sess.ID); err != nil {
		slog.Warn("daemon: project membership: add failed", "agent", sess.ID, "project", sess.ProjectID, "err", err)
	}
}

// removeProjectMembership drops a torn-down agent from its project's membership
// list, the delete-side mirror of addProjectMembership. Best-effort (removing an
// absent member is a no-op) and a silent no-op for a project-less agent or an
// unconfigured store. Call when an agent is deleted/archived (not merely
// terminated: a still-recorded orphaned/hibernated member is tolerated as a
// dangling id per spec §6.3, so terminate leaves membership intact).
func (s *Server) removeProjectMembership(sess *agentstore.Agent) {
	if s.projects == nil || sess == nil || sess.ProjectID == "" {
		return
	}
	if _, err := s.projects.RemoveAgentFromProject(sess.ProjectID, sess.ID); err != nil {
		slog.Warn("daemon: project membership: remove failed", "agent", sess.ID, "project", sess.ProjectID, "err", err)
	}
}

// resolvePipelineProjectID path-matches a freshly created pipeline's repo against
// the OPEN projects, the pipeline-side mirror of resolveProjectID for agents.
// The caller has already applied the higher-precedence sources (an explicit
// request-body project_id, then the YAML spec's project_id); this only fills the
// still-empty case by location. Returns "" when no projects store is wired or no
// open project matches and no repo is given — the pipeline is then project-less and joins no membership
// list. With no open match the repo is auto-registered (or its closed project
// reopened) via ensureOpenProject.
func (s *Server) resolvePipelineProjectID(p *pipeline.Pipeline) string {
	if p == nil || s.projects == nil {
		return ""
	}
	dir := normalizeProjectDir(p.Repo)
	if dir == "" {
		return ""
	}
	projs, err := s.projects.List()
	if err != nil {
		slog.Warn("daemon: pipeline membership: list projects failed", "pipeline", p.ID, "err", err)
		return ""
	}
	for _, proj := range projs {
		if projectstore.NormalizeStatus(proj.Status) != projectstore.StatusOpen {
			continue
		}
		if dir == proj.ID || (proj.Path != "" && dir == proj.Path) {
			return proj.ID
		}
	}
	return s.ensureProjectID(dir, p.ID)
}

// addPipelineMembership appends a created pipeline to its project's authoritative
// Project.pipelines[] list (spec D2/§3.1), the pipeline analogue of
// addProjectMembership. Best-effort: the add de-duplicates (a repeat is a no-op)
// and a failure is logged, never fatal — the pipeline keeps its ProjectID back-ref
// regardless. A project-less pipeline (empty ProjectID) or an unconfigured projects
// store is a silent no-op. Call AFTER a successful pstore.Create.
func (s *Server) addPipelineMembership(p *pipeline.Pipeline) {
	if s.projects == nil || p == nil || p.ProjectID == "" {
		return
	}
	if _, err := s.projects.AddPipelineToProject(p.ProjectID, p.ID); err != nil {
		slog.Warn("daemon: pipeline membership: add failed", "pipeline", p.ID, "project", p.ProjectID, "err", err)
	}
}

// removePipelineMembership drops a deleted pipeline from its project's
// Project.pipelines[] list, the delete-side mirror of addPipelineMembership.
// Best-effort (removing an absent member is a no-op) and a silent no-op for a
// project-less pipeline or an unconfigured store. Call when a pipeline is deleted.
func (s *Server) removePipelineMembership(p *pipeline.Pipeline) {
	if s.projects == nil || p == nil || p.ProjectID == "" {
		return
	}
	if _, err := s.projects.RemovePipelineFromProject(p.ProjectID, p.ID); err != nil {
		slog.Warn("daemon: pipeline membership: remove failed", "pipeline", p.ID, "project", p.ProjectID, "err", err)
	}
}

// addPlanMembership appends a plan to its project's authoritative plans[] list.
// Best-effort: a failure is logged and never fails the primary plan operation.
func (s *Server) addPlanMembership(planID, projectID string) {
	if s.projects == nil || planID == "" || projectID == "" {
		return
	}
	if _, err := s.projects.AddPlanToProject(projectID, planID); err != nil {
		slog.Warn("daemon: plan membership: add failed", "plan", planID, "project", projectID, "err", err)
	}
}

// removePlanMembership drops a plan from its project's authoritative plans[]
// list. Best-effort: a failure is logged and never fails the primary operation.
func (s *Server) removePlanMembership(planID, projectID string) {
	if s.projects == nil || planID == "" || projectID == "" {
		return
	}
	if _, err := s.projects.RemovePlanFromProject(projectID, planID); err != nil {
		slog.Warn("daemon: plan membership: remove failed", "plan", planID, "project", projectID, "err", err)
	}
}

// addAutopilotMembership appends an autopilot run to its project's authoritative
// autopilots[] list. Best-effort: a failure is logged and never fails the
// primary run operation.
func (s *Server) addAutopilotMembership(runID, projectID string) {
	if s.projects == nil || runID == "" || projectID == "" {
		return
	}
	if _, err := s.projects.AddAutopilotToProject(projectID, runID); err != nil {
		slog.Warn("daemon: autopilot membership: add failed", "run", runID, "project", projectID, "err", err)
	}
}

// removeAutopilotMembership drops an autopilot run from its project's
// authoritative autopilots[] list. Best-effort: a failure is logged and never
// fails the primary operation.
func (s *Server) removeAutopilotMembership(runID, projectID string) {
	if s.projects == nil || runID == "" || projectID == "" {
		return
	}
	if _, err := s.projects.RemoveAutopilotFromProject(projectID, runID); err != nil {
		slog.Warn("daemon: autopilot membership: remove failed", "run", runID, "project", projectID, "err", err)
	}
}
