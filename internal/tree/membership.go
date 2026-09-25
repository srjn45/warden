package tree

import (
	"path/filepath"
	"strings"

	"github.com/srjn45/warden/internal/autopilot"
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

// agentForest splits agent sessions into root agents plus a parent→children map,
// preferring the STORED parent/child edges (spec D3) over path inference. A child
// nests under its parent whenever a stored edge connects them in the same
// canonical directory. A cross-project child remains a root, where renderers can
// show its lineage backlink without moving it out of its own project.
// Only legacy rows that carry no edge at all (empty parent_id and absent from every
// child_agents[]) are treated by path — here that just means they are roots.
//
// Edge precedence follows spec §6.1/§6.3: the backward edge (child.parent_id) wins
// when its target is present in the set; the forward edge (parent.child_agents[])
// fills in a child whose own parent_id is empty or dangling. An orphan whose parent
// is absent from the set is promoted to a root so it never vanishes, and a self- or
// cyclic edge is broken by rendering the node as a root (dangling ids tolerated).
func agentForest(sessions []*store.Session) (roots []*store.Session, childrenByParent map[string][]*store.Session) {
	byID := make(map[string]*store.Session, len(sessions))
	for _, s := range sessions {
		byID[s.ID] = s
	}

	// Resolve each child's effective parent from the stored edges.
	parentOf := make(map[string]string, len(sessions))
	// Backward edge: child.parent_id (authoritative when its target is present
	// in the same project/directory).
	for _, s := range sessions {
		parent := byID[s.ParentID]
		if s.ParentID != "" && s.ParentID != s.ID && parent != nil && sessionDir(s) == sessionDir(parent) {
			parentOf[s.ID] = s.ParentID
		}
	}
	// Forward edge: parent.child_agents[] fills in a child whose own parent_id is
	// empty or dangling. The backward edge wins when both are present.
	for _, p := range sessions {
		for _, cid := range p.ChildAgents {
			if cid == "" || cid == p.ID {
				continue
			}
			if _, resolved := parentOf[cid]; resolved {
				continue
			}
			if child := byID[cid]; child != nil && sessionDir(child) == sessionDir(p) {
				parentOf[cid] = p.ID
			}
		}
	}

	childrenByParent = map[string][]*store.Session{}
	for _, s := range sessions {
		if pid, ok := parentOf[s.ID]; ok && chainReachesRoot(s.ID, parentOf) {
			childrenByParent[pid] = append(childrenByParent[pid], s)
			continue
		}
		roots = append(roots, s)
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
