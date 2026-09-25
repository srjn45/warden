package daemon

import (
	"context"
	"errors"
	"log/slog"

	"github.com/srjn45/warden/internal/store"
)

// Agent child-edge maintenance (docs/specs/2026-09-25-project-entity-hierarchy.md
// D3/§6.1). The parent→child relationship is stored on BOTH ends: the child's
// ParentID back-ref (stamped by lifecycle at spawn) and the parent's ChildAgents[]
// forward edge. These helpers keep the forward edge consistent at the two write
// edges this phase owns — spawn/adopt (add) and delete/archive (remove) — plus a
// reparent primitive that moves a child between parents. The "complete id list"
// on the parent is the edge of record when the two ends disagree (§6.1); the
// per-session ParentID is the mirror.
//
// Only USER-FACING spawned sub-agents populate a parent's ChildAgents[] (D5/§6.2):
// pipeline job agents (they carry PipelineID/JobID and are reached through the
// owning pipeline, never listed here) and terminals (leaf members, §6.4) are
// excluded. Autopilot workers carry run back-refs, not a parent_id (their
// ParentID is cleared before spawn), so they never reach these helpers.

// childOfParent reports whether sess should be recorded on its parent's
// ChildAgents[] forward edge: it must name a parent, not be a terminal (§6.4),
// and not be a pipeline job agent (D5). A self-parent (already prevented at
// spawn) is likewise excluded so a record can never list itself.
func childOfParent(sess *store.Session) bool {
	return sess != nil &&
		sess.ParentID != "" &&
		sess.ParentID != sess.ID &&
		!sess.IsTerminal() &&
		sess.PipelineID == "" &&
		sess.JobID == ""
}

// addChildEdge appends an inserted session to its parent agent's authoritative
// ChildAgents[] forward edge (spec D3/§6.1), the child-edge mirror of
// addProjectMembership. Best-effort: the append de-duplicates (a repeat is a
// no-op, so recovery/re-insert is idempotent) and a failure is logged, never
// fatal — the child keeps its ParentID back-ref regardless, and the two ends are
// reconciled with this list as the source of truth. A root spawn (empty
// ParentID), a job agent, or a terminal is a silent no-op (childOfParent).
// A missing parent record is tolerated (dangling back-ref, §6.3). A terminal
// parent is rejected on BOTH ends (§6.4 — terminals are leaf members and never
// own children): the forward edge is not written and the child's ParentID
// back-ref is cleared. Call AFTER a successful store.Insert.
func (s *Server) addChildEdge(ctx context.Context, sess *store.Session) {
	if !childOfParent(sess) {
		return
	}
	if parent, err := s.store.Get(ctx, sess.ParentID); err == nil && parent.IsTerminal() {
		s.clearTerminalParentBackRef(ctx, sess)
		return
	}
	if err := s.store.Update(ctx, sess.ParentID, func(p *store.Session) error {
		if p.IsTerminal() {
			return nil // race: became terminal between Get and Update
		}
		p.ChildAgents = appendUnique(p.ChildAgents, sess.ID)
		return nil
	}); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return // parent gone (dangling back-ref tolerated, §6.3)
		}
		slog.Warn("daemon: child edge: add failed", "child", sess.ID, "parent", sess.ParentID, "err", err)
	}
}

// clearTerminalParentBackRef drops a child's ParentID when it named a terminal,
// so neither end claims a terminal owner (§6.4). Best-effort.
func (s *Server) clearTerminalParentBackRef(ctx context.Context, sess *store.Session) {
	if sess == nil || sess.ID == "" {
		return
	}
	parentID := sess.ParentID
	if err := s.store.Update(ctx, sess.ID, func(c *store.Session) error {
		c.ParentID = ""
		return nil
	}); err != nil && !errors.Is(err, store.ErrNotFound) {
		slog.Warn("daemon: child edge: clear terminal parent back-ref failed", "child", sess.ID, "parent", parentID, "err", err)
		return
	}
	sess.ParentID = ""
}

// removeChildEdge drops a torn-down session from its parent agent's ChildAgents[]
// list, the delete-side mirror of addChildEdge. Best-effort (removing an absent
// id is a no-op) and a silent no-op for a root/job/terminal session. Call when a
// session is deleted/archived (not merely terminated: a still-recorded
// orphaned/hibernated child stays on the list as a tolerated dangling id per
// §6.3, and a tombstoned parent that keeps its record keeps its own edge).
func (s *Server) removeChildEdge(ctx context.Context, sess *store.Session) {
	if !childOfParent(sess) {
		return
	}
	if err := s.store.Update(ctx, sess.ParentID, func(p *store.Session) error {
		p.ChildAgents = removeString(p.ChildAgents, sess.ID)
		return nil
	}); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return
		}
		slog.Warn("daemon: child edge: remove failed", "child", sess.ID, "parent", sess.ParentID, "err", err)
	}
}

// reparentChildEdge moves childID's forward edge from oldParentID to newParentID,
// keeping both ends consistent under a reparent (spec §6.1): the id is removed
// from the old parent's ChildAgents[] and appended to the new parent's. Either
// parent may be empty (a reparent to/from a root spawn) or missing (dangling,
// §6.3), in which case only the present side is touched. Caller updates the
// child's ParentID back-ref. Best-effort; both sides are attempted independently
// so a failure on one still applies the other.
func (s *Server) reparentChildEdge(ctx context.Context, childID, oldParentID, newParentID string) {
	if childID == "" || oldParentID == newParentID {
		return
	}
	if oldParentID != "" {
		if err := s.store.Update(ctx, oldParentID, func(p *store.Session) error {
			p.ChildAgents = removeString(p.ChildAgents, childID)
			return nil
		}); err != nil && !errors.Is(err, store.ErrNotFound) {
			slog.Warn("daemon: child edge: reparent detach failed", "child", childID, "parent", oldParentID, "err", err)
		}
	}
	if newParentID != "" && newParentID != childID {
		if np, err := s.store.Get(ctx, newParentID); err == nil && np.IsTerminal() {
			// §6.4: reject terminal parents on both ends — detach already applied
			// above; clear the child's back-ref so it becomes a root.
			_ = s.store.Update(ctx, childID, func(c *store.Session) error {
				c.ParentID = ""
				return nil
			})
			return
		}
		if err := s.store.Update(ctx, newParentID, func(p *store.Session) error {
			if p.IsTerminal() {
				return nil
			}
			p.ChildAgents = appendUnique(p.ChildAgents, childID)
			return nil
		}); err != nil && !errors.Is(err, store.ErrNotFound) {
			slog.Warn("daemon: child edge: reparent attach failed", "child", childID, "parent", newParentID, "err", err)
		}
	}
}

// appendUnique returns list with v appended only if not already present, so the
// forward edge never holds duplicate ids (idempotent adds).
func appendUnique(list []string, v string) []string {
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}

// removeString returns list with every occurrence of v dropped, preserving order.
// Retains a non-nil empty result: an authoritative list must not become legacy.
func removeString(list []string, v string) []string {
	out := list[:0:0]
	for _, x := range list {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}
