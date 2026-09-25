package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"sort"

	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/projectstore"
	"github.com/srjn45/warden/internal/store"
)

// Project membership backfill / reconciliation
// (docs/specs/2026-09-25-project-entity-hierarchy.md D2/§3.1, §5, §6).
//
// The live edges (spawn/delete) are maintained incrementally by
// project_membership.go, but a store written before those lists existed — or one
// that drifted — needs a one-shot repair. This is that repair. It treats the
// members' back-refs as the source of truth for a migration (the project lists do
// not yet exist, so they cannot be) and reconciles the two ends in two passes:
//
//  1. Stamp — a session or pipeline with an empty ProjectID is path-matched
//     against the OPEN projects and, on a match, gains its back-ref. A closed
//     (hibernated) project is never auto-matched: opening it is what re-associates
//     its members. This mirrors the live-spawn rules in resolveProjectID /
//     resolvePipelineProjectID.
//  2. Rebuild — every project's authoritative agents[]/pipelines[]/terminals[]
//     lists are recomputed from a full scan of the (now-stamped) back-refs and
//     rewritten, sorted and de-duplicated, but only when they differ from what is
//     stored.
//
// Runs at daemon boot (cli/daemon.go, in-process, no writer contention) and, for
// manual repair when the daemon is down, from `warden doctor --reconcile-membership`.

// MembershipReconcileReport summarizes one backfill/reconcile sweep.
type MembershipReconcileReport struct {
	// SessionsStamped is the number of sessions that gained a project_id back-ref
	// via path-match (were previously project-less).
	SessionsStamped int
	// PipelinesStamped is the number of pipelines that gained a project_id back-ref
	// via path-match.
	PipelinesStamped int
	// ProjectsRebuilt is the number of projects whose membership lists were rewritten
	// (i.e. differed from the recomputed set).
	ProjectsRebuilt int
}

// Changed reports whether the sweep made any write. A fully-reconciled store
// yields an all-zero report (the idempotent no-op case).
func (r MembershipReconcileReport) Changed() bool {
	return r.SessionsStamped > 0 || r.PipelinesStamped > 0 || r.ProjectsRebuilt > 0
}

// ReconcileProjectMembership performs the two-pass backfill/repair described above
// against the given stores. It is idempotent: a second run over an
// already-reconciled store makes no writes and returns an all-zero report. It is
// best-effort per row — a single session/pipeline/project store failure is logged
// and skipped, never aborting the sweep — but a failure to list projects or
// sessions/pipelines up front is returned, since the sweep cannot proceed without
// them. A nil projects store (unconfigured) or nil pipeline store (pipelines
// unused) is tolerated: the corresponding pass is skipped.
func ReconcileProjectMembership(ctx context.Context, sstore store.Store, pstore *pipeline.Store, projects *projectstore.Store) (MembershipReconcileReport, error) {
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

	// Pass 1a: stamp project-less sessions by path-matching the open projects.
	var sessions []*store.Session
	if sstore != nil {
		sessions, err = sstore.List(ctx)
		if err != nil {
			return rep, fmt.Errorf("list sessions: %w", err)
		}
		for _, sess := range sessions {
			if sess == nil || sess.ProjectID != "" {
				continue
			}
			pid := matchOpenProjectForSession(sess, projs)
			if pid == "" {
				continue
			}
			if err := sstore.Update(ctx, sess.ID, func(s *store.Session) error {
				if s.ProjectID == "" {
					s.ProjectID = pid
				}
				return nil
			}); err != nil {
				slog.Warn("daemon: membership reconcile: stamp session failed", "agent", sess.ID, "project", pid, "err", err)
				continue
			}
			sess.ProjectID = pid // reflect the write for the rebuild scan below
			rep.SessionsStamped++
		}
	}

	// Pass 1b: stamp project-less pipelines by path-matching the open projects.
	var pipelines []*pipeline.Pipeline
	if pstore != nil {
		pipelines, err = pstore.List()
		if err != nil {
			return rep, fmt.Errorf("list pipelines: %w", err)
		}
		for _, p := range pipelines {
			if p == nil || p.ProjectID != "" {
				continue
			}
			pid := matchOpenProjectForDir(normalizeProjectDir(p.Repo), projs)
			if pid == "" {
				continue
			}
			if err := pstore.Update(p.ID, func(up *pipeline.Pipeline) {
				if up.ProjectID == "" {
					up.ProjectID = pid
				}
			}); err != nil {
				slog.Warn("daemon: membership reconcile: stamp pipeline failed", "pipeline", p.ID, "project", pid, "err", err)
				continue
			}
			p.ProjectID = pid // reflect the write for the rebuild scan below
			rep.PipelinesStamped++
		}
	}

	// Pass 2: rebuild every project's membership lists from the back-refs (the
	// source of truth), rewriting only the projects whose stored lists differ.
	// Every project is rebuilt, including closed ones: a hibernated project's
	// members keep their ProjectID and belong in its lists.
	for _, proj := range projs {
		agents, terminals := membersForProject(proj.ID, sessions)
		pipes := pipelinesForProject(proj.ID, pipelines)
		if stringsEqual(agents, proj.Agents) &&
			stringsEqual(terminals, proj.Terminals) &&
			stringsEqual(pipes, proj.Pipelines) {
			continue
		}
		proj.Agents = agents
		proj.Pipelines = pipes
		proj.Terminals = terminals
		if err := projects.Upsert(proj); err != nil {
			slog.Warn("daemon: membership reconcile: rebuild project failed", "project", proj.ID, "err", err)
			continue
		}
		rep.ProjectsRebuilt++
	}
	return rep, nil
}

// matchOpenProjectForSession returns the id of the OPEN project a project-less
// session path-matches, or "" for none. Mirrors resolveProjectID's open-only rule.
func matchOpenProjectForSession(sess *store.Session, projs []projectstore.Project) string {
	for _, p := range projs {
		if projectstore.NormalizeStatus(p.Status) != projectstore.StatusOpen {
			continue
		}
		if sessionInProject(sess, p) {
			return p.ID
		}
	}
	return ""
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

// membersForProject partitions the sessions whose ProjectID equals projectID into
// the project's agent and terminal id lists (each sorted + de-duplicated). Job
// agents are ordinary sessions here — they are members of their project like any
// other; the spec's D5 exclusion is about an agent's child_agents[], not the
// project's flat agents[] membership.
func membersForProject(projectID string, sessions []*store.Session) (agents, terminals []string) {
	for _, sess := range sessions {
		if sess == nil || sess.ProjectID != projectID {
			continue
		}
		if sess.IsTerminal() {
			terminals = append(terminals, sess.ID)
		} else {
			agents = append(agents, sess.ID)
		}
	}
	return sortedDedupe(agents), sortedDedupe(terminals)
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

// sortedDedupe returns ids sorted ascending with adjacent duplicates removed, or
// nil for an empty input. A canonical order makes the rebuild deterministic (so
// the diff-before-write stays idempotent regardless of store scan order), and nil
// (not an empty slice) round-trips cleanly through the list fields' omitempty.
func sortedDedupe(ids []string) []string {
	if len(ids) == 0 {
		return nil
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

// stringsEqual reports whether a and b hold the same elements in the same order,
// treating nil and an empty slice as equal.
func stringsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
