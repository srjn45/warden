package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"sort"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/planstore"
	"github.com/srjn45/warden/internal/projectstore"
)

// Project membership reconciliation (spec D2/§6) treats non-nil forward lists
// as authoritative, including empty lists. Only nil legacy lists are backfilled
// from reverse edges and open-project path matches. Existing order and dangling
// IDs survive every sweep. Conflicting forward claims are reported as ambiguous_membership
// conflicts and left untouched (no reverse restamp, no list rewrite).
//
// Plans[] is backfilled by scanning the plan store (plan ProjectID back-refs).
// Autopilots[] is never inferred here — completed or deleted executors must not
// be resurrected into membership; live Autopilot membership is stamped at
// run-create time only.
//
// Runs at startup or via doctor while the daemon is down (no writer contention).

// MembershipReconcileReport summarizes one backfill/reconcile sweep.
type MembershipReconcileReport struct {
	// SessionsStamped counts repaired session ProjectID back-refs.
	SessionsStamped int
	// PipelinesStamped counts repaired pipeline ProjectID back-refs.
	PipelinesStamped int
	// ProjectsRebuilt counts projects with newly backfilled legacy lists.
	ProjectsRebuilt int
	// Conflicts lists identity ambiguities left untouched (see
	// detectIdentityConflicts). They never count as changes.
	Conflicts []IdentityConflict
}

// Changed reports whether the sweep made any write. A fully-reconciled store
// yields an all-zero report (the idempotent no-op case).
func (r MembershipReconcileReport) Changed() bool {
	return r.SessionsStamped > 0 || r.PipelinesStamped > 0 || r.ProjectsRebuilt > 0
}

// ReconcileProjectMembership performs the backfill/repair described above
// against the given stores. It is idempotent: a second run over an
// already-reconciled store makes no writes and returns an all-zero report. It is
// best-effort per row — a single session/pipeline/project store failure is logged
// and skipped (legacy backfill is deferred if reverse repair fails), but a
// failure to list projects or
// sessions/pipelines/plans up front is returned, since the sweep cannot proceed without
// them. A nil projects store (unconfigured) or nil pipeline/plan store (unused) is
// tolerated: the corresponding pass is skipped.
func ReconcileProjectMembership(ctx context.Context, sstore *agentstore.Store, pstore *pipeline.Store, plans *planstore.Store, projects *projectstore.Store) (MembershipReconcileReport, error) {
	var rep MembershipReconcileReport
	if projects == nil {
		return rep, nil
	}
	projs, err := projects.List()
	if err != nil {
		return rep, fmt.Errorf("list projects: %w", err)
	}
	if len(projs) == 0 {
		return rep, nil
	}

	// Read all sources before writing. An unavailable store must not turn an
	// unknown legacy list into an authoritative empty list.
	var sessions, archived []*agentstore.Agent
	if sstore != nil {
		sessions, err = sstore.List(ctx)
		if err != nil {
			return rep, fmt.Errorf("list sessions: %w", err)
		}
		archived, err = sstore.ListClosed(ctx)
		if err != nil {
			return rep, fmt.Errorf("list archived sessions: %w", err)
		}
	}
	var pipelines []*pipeline.Pipeline
	if pstore != nil {
		pipelines, err = pstore.List()
		if err != nil {
			return rep, fmt.Errorf("list pipelines: %w", err)
		}
	}
	var planRows []*planstore.Plan
	if plans != nil {
		planRows, err = plans.List(ctx)
		if err != nil {
			return rep, fmt.Errorf("list plans: %w", err)
		}
	}
	sort.Slice(projs, func(i, j int) bool { return projs[i].ID < projs[j].ID })
	// Only verified identities are reconciled; conflicted ids are reported and
	// skipped everywhere below. Archived records are read-only here.
	rep.Conflicts = detectIdentityConflicts(sessions, archived, projs)
	conflicted := conflictedIDs(rep.Conflicts)
	agentOwners, pipeOwners := map[string]string{}, map[string]string{}
	byID := make(map[string]projectstore.Project, len(projs))
	claim := func(owners map[string]string, ids []string, pid string) {
		for _, id := range ids {
			if _, exists := owners[id]; !exists {
				owners[id] = pid
			}
		}
	}
	for _, proj := range projs {
		byID[proj.ID] = proj
		claim(agentOwners, proj.Agents, proj.ID)
		claim(pipeOwners, proj.Pipelines, proj.ID)
	}
	repairFailed := false
	for _, sess := range sessions {
		if sess == nil || conflicted[sess.ID] {
			continue
		}
		owners := agentOwners
		list := func(p projectstore.Project) []string { return p.Agents }
		pid := sess.ProjectID
		if owner, ok := owners[sess.ID]; ok {
			pid = owner
		} else if p, ok := byID[pid]; ok && list(p) != nil {
			pid = "" // the authoritative container excludes this reverse claim
		}
		if pid == "" && sess.ProjectID == "" {
			// Path matching is migration-only, never a way to repopulate [] lists.
			for _, p := range projs {
				if list(p) == nil && projectstore.NormalizeStatus(p.Status) == projectstore.StatusOpen && sessionInProject(sess, p) {
					pid = p.ID
					break
				}
			}
		}
		if pid == sess.ProjectID {
			continue
		}
		if err := sstore.Update(ctx, sess.ID, func(s *agentstore.Agent) error { s.ProjectID = pid; return nil }); err != nil {
			repairFailed = true
			slog.Warn("daemon: membership reconcile: repair session failed", "agent", sess.ID, "err", err)
			continue
		}
		sess.ProjectID = pid
		rep.SessionsStamped++
	}
	for _, p := range pipelines {
		if p == nil {
			continue
		}
		pid := p.ProjectID
		if owner, ok := pipeOwners[p.ID]; ok {
			pid = owner
		} else if proj, ok := byID[pid]; ok && proj.Pipelines != nil {
			pid = ""
		}
		if pid == "" && p.ProjectID == "" {
			for _, proj := range projs {
				if proj.Pipelines == nil && matchOpenProjectForDir(normalizeProjectDir(p.Repo), []projectstore.Project{proj}) != "" {
					pid = proj.ID
					break
				}
			}
		}
		if pid == p.ProjectID {
			continue
		}
		if err := pstore.Update(p.ID, func(up *pipeline.Pipeline) { up.ProjectID = pid }); err != nil {
			repairFailed = true
			slog.Warn("daemon: membership reconcile: repair pipeline failed", "pipeline", p.ID, "err", err)
			continue
		}
		p.ProjectID = pid
		rep.PipelinesStamped++
	}
	// Retry legacy backfill later if a reverse repair failed; otherwise a stale
	// reverse edge could become a new, competing authoritative forward claim.
	if repairFailed {
		return rep, nil
	}
	for _, proj := range projs {
		changed := false
		if sstore != nil {
			agents := membersForProject(proj.ID, sessionsExcluding(sessions, conflicted))
			if proj.Agents == nil {
				proj.Agents = agents
				changed = true
			}
		}
		if pstore != nil && proj.Pipelines == nil {
			proj.Pipelines = pipelinesForProject(proj.ID, pipelines)
			changed = true
		}
		if plans != nil && proj.Plans == nil {
			// Preserve first-seen order from the plan store scan after de-dupe.
			// Do NOT invent Autopilots[] — completed/deleted executors stay out.
			proj.Plans = plansForProject(proj.ID, planRows)
			changed = true
		}
		if !changed {
			continue
		}
		if err := projects.Upsert(proj); err != nil {
			slog.Warn("daemon: membership reconcile: backfill project failed", "project", proj.ID, "err", err)
			continue
		}
		rep.ProjectsRebuilt++
	}
	return rep, nil
}

// matchOpenProjectForDir returns the id of the OPEN project whose canonical
// id/path equals the normalized dir, or "" for none. Mirrors
// resolvePipelineProjectID's open-only rule. An empty dir never matches.
func matchOpenProjectForDir(dir string, projs []projectstore.Project) string {
	if dir == "" {
		return ""
	}
	for _, p := range projs {
		if projectstore.NormalizeStatus(p.Status) != projectstore.StatusOpen {
			continue
		}
		if dir == p.ID || (p.Path != "" && dir == p.Path) {
			return p.ID
		}
	}
	return ""
}

// membersForProject partitions the agents whose ProjectID equals projectID into
// the project's agent id list (sorted + de-duplicated).
func membersForProject(projectID string, sessions []*agentstore.Agent) []string {
	var agents []string
	for _, sess := range sessions {
		if sess == nil || sess.ProjectID != projectID {
			continue
		}
		agents = append(agents, sess.ID)
	}
	return sortedDedupe(agents)
}

// pipelinesForProject returns the sorted, de-duplicated id list of pipelines whose
// ProjectID equals projectID.
func pipelinesForProject(projectID string, pipelines []*pipeline.Pipeline) []string {
	var out []string
	for _, p := range pipelines {
		if p == nil || p.ProjectID != projectID {
			continue
		}
		out = append(out, p.ID)
	}
	return sortedDedupe(out)
}

// plansForProject returns the de-duplicated id list of plans whose ProjectID
// equals projectID, preserving first-seen order from the plan-store listing.
func plansForProject(projectID string, plans []*planstore.Plan) []string {
	var out []string
	for _, p := range plans {
		if p == nil || p.ProjectID != projectID || p.ID == "" {
			continue
		}
		out = append(out, p.ID)
	}
	return firstSeenDedupe(out)
}

// sortedDedupe gives newly backfilled lists a deterministic order and an explicit
// empty value so migration completes even when no members exist.
func sortedDedupe(ids []string) []string {
	if len(ids) == 0 {
		return []string{}
	}
	sort.Strings(ids)
	out := ids[:1]
	for _, id := range ids[1:] {
		if id != out[len(out)-1] {
			out = append(out, id)
		}
	}
	return out
}

// firstSeenDedupe collapses blanks and duplicates while preserving encounter
// order — used for Plans[] so the plan-store listing order survives backfill.
func firstSeenDedupe(ids []string) []string {
	if len(ids) == 0 {
		return []string{}
	}
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// sessionsExcluding drops conflicted ids so backfill never trusts them.
func sessionsExcluding(sessions []*agentstore.Agent, skip map[string]bool) []*agentstore.Agent {
	if len(skip) == 0 {
		return sessions
	}
	out := make([]*agentstore.Agent, 0, len(sessions))
	for _, s := range sessions {
		if s != nil && !skip[s.ID] {
			out = append(out, s)
		}
	}
	return out
}
