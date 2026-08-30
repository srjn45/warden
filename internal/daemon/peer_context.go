package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"

	"github.com/srjn45/warden/internal/projectstore"
	"github.com/srjn45/warden/internal/store"
)

// PeerContext is the daemon-side provider wired into lifecycle as PeerContextFn
// (Project Groups Phase 3: Peer Awareness & Context Injection). Given an agent that
// is about to (re)launch, it returns the dynamic system-prompt addendum naming the
// Project Group the agent belongs to and the sibling orchestrators it can coordinate
// with via send_message — or "" when there is nothing to inject.
//
// It projects context ONLY for a live per-project orchestrator (role orchestrator)
// whose project is a member of some group; every other agent (workers, planners,
// ungrouped orchestrators) gets "". Peers are the OTHER live orchestrators across
// the group's member projects — self is always excluded. The set is recomputed from
// live store state on every call, so the addendum reflects the group as it stands at
// the moment of launch.
//
// Fail-open by contract (lifecycle treats the result as an additive hint): any store
// error is logged and degrades to "", never blocking the spawn.
func (s *Server) PeerContext(ctx context.Context, sess *store.Session) string {
	if s == nil || sess == nil || s.store == nil || s.projects == nil {
		return ""
	}
	if sess.Role != orchestratorRole {
		return ""
	}
	// The orchestrator must be pinned to a project (Phase 2 stamps ProjectID on
	// every auto-spawned orch); without it we cannot resolve group membership.
	if sess.ProjectID == "" {
		return ""
	}
	group, ok := s.groupForProject(sess.ProjectID)
	if !ok {
		return "" // project belongs to no group — nothing to coordinate
	}
	peers := s.livePeerOrchestrators(ctx, group, sess)
	return renderPeerContext(group.Name, peers)
}

// groupForProject returns the first group whose membership includes projectID
// (mirroring the TUI group-label rule: first group wins on overlap), and ok=false
// when the project is in no group or the group list cannot be read. ListGroups is
// name-then-id sorted, so "first" is deterministic across calls.
func (s *Server) groupForProject(projectID string) (projectstore.ProjectGroup, bool) {
	groups, err := s.projects.ListGroups()
	if err != nil {
		slog.Warn("daemon: peer-context: list groups failed", "project", projectID, "err", err)
		return projectstore.ProjectGroup{}, false
	}
	for _, g := range groups {
		if slices.Contains(g.ProjectIDs, projectID) {
			return g, true
		}
	}
	return projectstore.ProjectGroup{}, false
}

// livePeerOrchestrators returns the sorted, de-duplicated display names of every
// LIVE orchestrator agent (role orchestrator) whose project is a member of group,
// EXCLUDING self. A store read failure logs and yields no peers (fail-open). Names
// fall back to the agent id when a session has no name; ordering is lexical so the
// rendered addendum — and its tests — are deterministic.
func (s *Server) livePeerOrchestrators(ctx context.Context, group projectstore.ProjectGroup, self *store.Session) []string {
	all, err := s.store.List(ctx)
	if err != nil {
		slog.Warn("daemon: peer-context: list sessions failed", "agent", self.ID, "err", err)
		return nil
	}
	member := make(map[string]struct{}, len(group.ProjectIDs))
	for _, pid := range group.ProjectIDs {
		member[pid] = struct{}{}
	}
	seen := make(map[string]struct{})
	var names []string
	for _, o := range all {
		if o == nil || o.ID == self.ID { // exclude self (id is globally unique)
			continue
		}
		if o.Role != orchestratorRole || !liveStatus(o.Status) {
			continue
		}
		if _, ok := member[o.ProjectID]; !ok {
			continue
		}
		name := o.Name
		if name == "" {
			name = o.ID
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// renderPeerContext shapes the peer-awareness addendum from a group name and the
// (already sorted, self-excluded) peer orchestrator names. It returns "" for a blank
// group name (nothing to say). When the orchestrator is currently alone in its group
// it still learns the group membership; when peers exist it is told their names and
// that send_message reaches them directly. The wording avoids apostrophes to keep the
// single-quoted shell launch form clean, matching the sibling coordination hints.
func renderPeerContext(groupName string, peerNames []string) string {
	if strings.TrimSpace(groupName) == "" {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "This project belongs to Project Group %q, which coordinates a set of related projects, each with its own orchestrator.", groupName)
	if len(peerNames) == 0 {
		b.WriteString(" You are currently the only live orchestrator in this group; peers will appear as their projects are opened.")
		return b.String()
	}
	b.WriteString(" Peer orchestrators in this group you can coordinate with: ")
	b.WriteString(strings.Join(peerNames, ", "))
	b.WriteString(". Use send_message (mcp__warden__send_message) to reach a peer orchestrator directly when work spans projects in the group, instead of duplicating effort or waiting on the operator.")
	return b.String()
}
