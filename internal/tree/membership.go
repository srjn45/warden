package tree

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/projectstore"
	"github.com/srjn45/warden/internal/store"
)

// canonicalDir canonicalizes a repo/workdir path to a project root: made
// absolute, with a trailing "/.worktrees/<name>" suffix stripped so an agent in
// a worktree groups under its parent repo, not a pseudo-project. Mirrors the
// TUI's sourceDir/normalizePipelineDir. An empty input stays empty (→ the
// synthetic "No project" bucket).
func canonicalDir(dir string) string {
	if dir == "" {
		return ""
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	if idx := strings.Index(dir, string(filepath.Separator)+".worktrees"); idx != -1 {
		dir = dir[:idx]
	}
	return dir
}

// sessionDir is the grouping directory for a session: its Repo (set for
// typed/worktree agents), else its Workdir (the caller cwd), canonicalized.
// Empty when neither is known.
func sessionDir(s *store.Session) string {
	dir := s.Repo
	if dir == "" {
		dir = s.Workdir
	}
	return canonicalDir(dir)
}

// resolveGroupKey maps an entity's (projectID, canonicalized dir) to the group
// key it renders under (spec §8): a matching project's id (open OR closed), else
// the bare dir (a loose group), else "" (the synthetic "No project" bucket when
// there is no location). openByKey/closedByKey index every project by BOTH its
// id and its path → the project id.
//
// This is the LEGACY path/back-ref fallback. Prefer resolveMembershipKey when the
// caller has the project's stored agents[]/pipelines[]/terminals[] lists — those
// are the membership of record (spec D2/§6.1) and win over a contradictory
// ProjectID or path.
//
// This differs from the TUI's resolveGroupKey on one point (spec §16 D4 / Q-T5):
// a match to a CLOSED project keeps the item under that project (later marked
// closed) rather than folding it into Ungrouped. The service keeps closed
// projects and lets each client choose to dim or hide them.
func resolveGroupKey(projectID, dir string, openByKey, closedByKey map[string]string) string {
	if projectID != "" {
		if k, ok := openByKey[projectID]; ok {
			return k
		}
		if k, ok := closedByKey[projectID]; ok {
			return k
		}
	}
	if dir == "" {
		return ""
	}
	if k, ok := openByKey[dir]; ok {
		return k
	}
	if k, ok := closedByKey[dir]; ok {
		return k
	}
	return dir // loose dir group
}

// membershipKind selects which Project membership list to consult.
type membershipKind int

const (
	membershipAgents membershipKind = iota
	membershipPipelines
	membershipTerminals
)

// membershipList returns the project's stored id list for kind. A nil return
// means the field was never written (legacy); a non-nil empty slice is an
// explicit empty contract.
func membershipList(p projectstore.Project, kind membershipKind) []string {
	switch kind {
	case membershipAgents:
		return p.Agents
	case membershipPipelines:
		return p.Pipelines
	case membershipTerminals:
		return p.Terminals
	}
	return nil
}

// resolveMembershipKey returns the group key for an entity using the project's
// stored membership lists as the authority (spec D2/§6.1). The container list
// wins over contradictory ProjectID/path:
//
//  1. If one or more projects list entityID, the lexicographically smallest
//     project id wins (deterministic multi-list conflict).
//  2. Otherwise ProjectID/path are honored ONLY when the candidate project's
//     list is nil (legacy missing field). A non-nil empty list or a non-nil
//     list that excludes the entity is an explicit contract — the entity must
//     NOT regain membership via back-ref or path.
func resolveMembershipKey(
	entityID, projectID, dir string,
	kind membershipKind,
	projects []projectstore.Project,
	openByKey, closedByKey map[string]string,
) string {
	projectByID := make(map[string]projectstore.Project, len(projects))
	for _, p := range projects {
		projectByID[p.ID] = p
	}

	var listed []string
	for _, p := range projects {
		for _, id := range membershipList(p, kind) {
			if id == entityID {
				listed = append(listed, p.ID)
				break
			}
		}
	}
	if len(listed) > 0 {
		sort.Strings(listed)
		return listed[0]
	}

	// Not listed. Legacy ProjectID only when that project's list is nil.
	if projectID != "" {
		if p, ok := projectByID[projectID]; ok {
			if membershipList(p, kind) == nil {
				return projectID
			}
			// Non-nil contract excludes this entity — ignore ProjectID.
		} else if k, ok := openByKey[projectID]; ok {
			if membershipList(projectByID[k], kind) == nil {
				return k
			}
		} else if k, ok := closedByKey[projectID]; ok {
			if membershipList(projectByID[k], kind) == nil {
				return k
			}
		}
	}

	// Path: only land under a registered project whose list is nil (legacy).
	// If the path matches a project with a non-nil contract that excluded us,
	// do NOT return the path (it often equals the project id and would re-home
	// the entity under that project via the root builder) — fall to No-project.
	if dir == "" {
		return ""
	}
	if k, ok := openByKey[dir]; ok {
		if membershipList(projectByID[k], kind) == nil {
			return k
		}
		return ""
	}
	if k, ok := closedByKey[dir]; ok {
		if membershipList(projectByID[k], kind) == nil {
			return k
		}
		return ""
	}
	return dir // loose dir group (no registered project at this path)
}

// agentForest splits agent sessions into root agents plus a parent→children map
// from STORED edges (spec D3), ignoring path. Spec §6.1: the container's
// child_agents[] list is the membership of record when it disagrees with
// parent_id — forward edges win over backward. parent_id is consulted only for
// children that no present parent's child_agents[] claims. When multiple parents
// list the same child, the lexicographically smallest parent id wins. Children
// under a parent are emitted in that parent's child_agents[] order (list order
// preserved); parent_id-only extras follow in id order. Cycles break to roots;
// dangling ids are tolerated.
func agentForest(sessions []*store.Session) (roots []*store.Session, childrenByParent map[string][]*store.Session) {
	byID := make(map[string]*store.Session, len(sessions))
	for _, s := range sessions {
		byID[s.ID] = s
	}

	parentOf := make(map[string]string, len(sessions))
	// Forward first: container child_agents[] wins (spec §6.1).
	forwardClaim := make(map[string]string)
	for _, p := range sessions {
		for _, cid := range p.ChildAgents {
			if cid == "" || cid == p.ID || byID[cid] == nil {
				continue // dangling tolerated
			}
			if prev, ok := forwardClaim[cid]; ok && prev <= p.ID {
				continue
			}
			forwardClaim[cid] = p.ID
		}
	}
	claimedForward := make(map[string]bool, len(forwardClaim))
	for cid, pid := range forwardClaim {
		parentOf[cid] = pid
		claimedForward[cid] = true
	}
	// Backward parent_id only when no forward claim listed the child.
	for _, s := range sessions {
		if claimedForward[s.ID] {
			continue
		}
		if s.ParentID != "" && s.ParentID != s.ID && byID[s.ParentID] != nil {
			parentOf[s.ID] = s.ParentID
		}
	}

	childrenByParent = map[string][]*store.Session{}
	placed := make(map[string]bool, len(sessions))
	// Emit forward-claimed children in each parent's child_agents[] order.
	for _, p := range sessions {
		for _, cid := range p.ChildAgents {
			if parentOf[cid] != p.ID || placed[cid] {
				continue
			}
			if !chainReachesRoot(cid, parentOf) {
				continue
			}
			childrenByParent[p.ID] = append(childrenByParent[p.ID], byID[cid])
			placed[cid] = true
		}
	}
	// parent_id-only children (no winning forward claim), stable by id.
	var extras []*store.Session
	for _, s := range sessions {
		if placed[s.ID] {
			continue
		}
		if _, ok := parentOf[s.ID]; ok && chainReachesRoot(s.ID, parentOf) {
			extras = append(extras, s)
			continue
		}
		roots = append(roots, s)
	}
	sort.Slice(extras, func(i, j int) bool { return extras[i].ID < extras[j].ID })
	for _, s := range extras {
		pid := parentOf[s.ID]
		childrenByParent[pid] = append(childrenByParent[pid], s)
		placed[s.ID] = true
	}
	return roots, childrenByParent
}

// chainReachesRoot reports whether following the parentOf chain from start ends at
// a node with no parent (a real root) rather than looping. A cyclic chain returns
// false so its members render as roots — nothing vanishes and the recursive subtree
// build cannot spin forever.
func chainReachesRoot(start string, parentOf map[string]string) bool {
	seen := make(map[string]bool, len(parentOf))
	cur := start
	for {
		next, ok := parentOf[cur]
		if !ok {
			return true
		}
		if seen[cur] {
			return false
		}
		seen[cur] = true
		cur = next
	}
}

// isLive reports whether a session status is non-terminal (still running or
// awaiting input). Mirrors the daemon's/TUI's liveStatus. Drives sibling
// ordering (spec §8: live agents before terminal-state ones).
func isLive(s store.Status) bool {
	switch s {
	case store.StatusSpawning, store.StatusWorking, store.StatusWaitingForInput, store.StatusIdle:
		return true
	}
	return false
}

// backendOr returns a session's backend id, defaulting to "claude" when empty
// (backend is omitempty, so pre-feature records carry no value). Mirrors the
// TUI's backendOr.
func backendOr(s *store.Session) string {
	if s.Backend == "" {
		return "claude"
	}
	return s.Backend
}

// terminalDisplayName derives a display name for a terminal session (spec §4).
// Prefers the explicit session Name when non-empty. Otherwise, formats as
// "<repoBase> ~ <branch>" or "<repoBase>" when Repo is known; falls back to
// the base name of Workdir, or "shell" if neither is set.
func terminalDisplayName(s *store.Session) string {
	if s.Name != "" {
		return s.Name
	}
	if s.Repo != "" {
		base := filepath.Base(s.Repo)
		if s.Branch != "" {
			return base + " ~ " + s.Branch
		}
		return base
	}
	if s.Workdir != "" {
		base := filepath.Base(s.Workdir)
		if s.Branch != "" {
			return base + " ~ " + s.Branch
		}
		return base
	}
	return "shell"
}

// sessionRunID extracts the autopilot run ID from a session, checking
// AutopilotRunID first, then legacy run tags.
func sessionRunID(s *store.Session) string {
	if s == nil {
		return ""
	}
	if s.AutopilotRunID != "" {
		return s.AutopilotRunID
	}
	for _, tag := range s.Tags {
		if strings.HasPrefix(tag, "run:") {
			return strings.TrimPrefix(tag, "run:")
		}
		if strings.HasPrefix(tag, "autopilot-run:") {
			return strings.TrimPrefix(tag, "autopilot-run:")
		}
	}
	return ""
}

// runSessionSlot resolves the autopilot lane slot for a session in a run.
func runSessionSlot(s *store.Session, r *autopilot.RunStatus) string {
	if s.AutopilotSlot != "" {
		return s.AutopilotSlot
	}
	if r.GuardianID != "" && s.ID == r.GuardianID {
		return store.AutopilotSlotGuardian
	}
	if r.Brain != nil && s.ID == r.Brain.AgentID {
		return store.AutopilotSlotManager
	}
	if s.Role == "autopilot" {
		return store.AutopilotSlotManager
	}
	return store.AutopilotSlotWorker
}
