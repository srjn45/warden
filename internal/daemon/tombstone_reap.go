package daemon

import (
	"context"
	"time"

	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/store"
)

// reapTombstones archives "tombstone" parents whose sub-tree has gone fully
// terminal, walking up the ParentID chain from startID (agent sub-tree
// grouping, phase 3).
//
// A tombstone is a non-live record that still anchors child records but has zero
// LIVE children. Phase 2 keeps such a parent ACTIVE (terminal status, tmux torn
// down) when it is deleted while children run; once its last live descendant
// ends there is nothing left to anchor, so it is archived (normal store.Archive,
// leaving it retrievable under closed/). Archiving a parent can in turn make ITS
// own parent reapable, so the walk climbs the chain.
//
// Conditions to reap a node:
//   - it exists and is non-live (terminal); a still-live parent anchors its
//     children and is left untouched;
//   - none of its children is live;
//   - at the ENTRY node, it has at least one child record — a childless terminal
//     agent is an ordinary "done" agent, left for the operator to delete. A
//     CLIMBED ancestor implicitly had a child (the node we just archived), so it
//     is reapable even after that archive leaves it with no active children.
//
// Best-effort: any store error stops the walk. A visited-set guards against a
// malformed parent cycle.
//
// alive reconfirms tmux liveness for a node whose status is orphaned before
// treating it as a genuine tombstone: orphaned status can lag reality for one
// tick (e.g. a daemon restart racing tmux enumeration mis-classifies a session
// that never actually died), and unlike done/errored — which only ever follow
// a confirmed teardown — orphaned is not a reliable signal on its own that the
// session is gone. A nil alive func is treated as "can't confirm, assume
// alive" (the conservative default: never archive a live agent's record).
func reapTombstones(ctx context.Context, st store.Store, startID string, alive func(ctx context.Context, tmuxSession string) bool) {
	seen := make(map[string]bool)
	climbed := false
	for id := startID; id != "" && !seen[id]; {
		seen[id] = true

		p, err := st.Get(ctx, id)
		if err != nil {
			return // already archived/removed, or store error
		}
		if liveStatus(p.Status) {
			return // a live parent still anchors the sub-tree
		}
		if p.Status == store.StatusOrphaned {
			stillAlive := true // can't confirm ⇒ assume alive, never archive blind
			if alive != nil {
				stillAlive = alive(ctx, p.TmuxSession)
			}
			if stillAlive {
				return // orphaned status was stale — the session never actually died
			}
		}

		all, err := st.List(ctx)
		if err != nil {
			return
		}
		hasChild, hasLiveChild := false, false
		for _, c := range all {
			if c.ParentID != id {
				continue
			}
			hasChild = true
			if liveStatus(c.Status) {
				hasLiveChild = true
				break
			}
		}
		if hasLiveChild {
			return // a live child still needs this anchor
		}
		if !hasChild && !climbed {
			return // entry leaf: an ordinary done agent, not a tombstone
		}

		next := p.ParentID
		if err := st.Archive(ctx, id); err != nil {
			return
		}
		climbed = true
		id = next
	}
}

// reapAutopilotManagers archives terminal autopilot manager records for runID
// once no live workers remain for that run. Workers are linked by back-ref, not
// parent_id, so manager deletes no longer tombstone (WP6).
func reapAutopilotManagers(ctx context.Context, st store.Store, runID string, alive func(ctx context.Context, tmuxSession string) bool) {
	if runID == "" {
		return
	}
	all, err := st.List(ctx)
	if err != nil {
		return
	}
	for _, s := range all {
		if autopilot.IsWorkerRecord(s) && liveStatus(s.Status) {
			return // a live worker still anchors the run
		}
	}
	for _, s := range all {
		if autopilot.SessionRunID(s) != runID || !autopilot.IsManagerRecord(s) {
			continue
		}
		if liveStatus(s.Status) {
			continue
		}
		if s.Status == store.StatusOrphaned {
			stillAlive := true
			if alive != nil {
				stillAlive = alive(ctx, s.TmuxSession)
			}
			if stillAlive {
				continue
			}
		}
		_ = st.Archive(ctx, s.ID)
	}
}

// reapAllTombstones is the safety-net sweep: it walks every non-live record and
// reaps any that have become a fully-terminal tombstone. This catches sub-trees
// whose last child ended via a path the lazy hook missed (e.g. the SessionEnd
// hook or an operator terminate). Best-effort.
func (s *Server) reapAllTombstones(ctx context.Context) {
	all, err := s.store.List(ctx)
	if err != nil {
		return
	}
	var alive func(ctx context.Context, tmuxSession string) bool
	if s.poller != nil {
		alive = s.poller.SessionAlive
	}
	for _, p := range all {
		if liveStatus(p.Status) {
			continue
		}
		reapTombstones(ctx, s.store, p.ID, alive)
		if rid := autopilot.SessionRunID(p); rid != "" && autopilot.IsManagerRecord(p) {
			reapAutopilotManagers(ctx, s.store, rid, alive)
		}
	}
}

// runTombstoneReapSweep runs reapAllTombstones once at startup and then on a
// ticker, so a tombstone never lingers after its sub-tree goes terminal even if
// the lazy reap hook didn't fire.
func (s *Server) runTombstoneReapSweep(ctx context.Context, interval time.Duration) {
	s.reapAllTombstones(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.reapAllTombstones(ctx)
		}
	}
}
