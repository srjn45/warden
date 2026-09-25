package daemon

import (
	"context"
	"testing"

	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// childAgents fetches a parent's forward-edge list from the store.
func childAgents(t *testing.T, st store.Store, id string) []string {
	t.Helper()
	p, err := st.Get(context.Background(), id)
	require.NoError(t, err)
	return p.ChildAgents
}

// TestChildEdgeInvariant is the bidirectional-edge invariant test (spec §6.1):
// every parent/child relationship is stored on BOTH ends. It drives the edge
// through spawn (add), delete (remove), and reparent (move) and asserts the two
// ends agree at each step: child.ParentID <-> parent.ChildAgents[] membership.
func TestChildEdgeInvariant(t *testing.T) {
	ctx := context.Background()
	st, err := store.NewFileStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { st.Close(ctx) })
	s := &Server{store: st}

	// assertBothEnds is the invariant: child names parent AND parent lists child
	// (or, for want=false, parent does NOT list child).
	assertBothEnds := func(childID, parentID string, want bool) {
		t.Helper()
		child, err := st.Get(ctx, childID)
		require.NoError(t, err)
		require.Equal(t, parentID, child.ParentID, "child back-ref")
		require.Equal(t, want, contains(childAgents(t, st, parentID), childID),
			"parent %s forward edge should contain %s = %v", parentID, childID, want)
	}

	parent := &store.Session{ID: "agent-parent", Status: store.StatusWorking}
	require.NoError(t, st.Insert(ctx, parent))

	// --- spawn: add edge ---
	child := &store.Session{ID: "agent-child", ParentID: "agent-parent", Status: store.StatusWorking}
	require.NoError(t, st.Insert(ctx, child))
	s.addChildEdge(ctx, child)
	assertBothEnds("agent-child", "agent-parent", true)

	// idempotent: a re-add (e.g. recovery re-insert) does not duplicate.
	s.addChildEdge(ctx, child)
	require.Equal(t, []string{"agent-child"}, childAgents(t, st, "agent-parent"))

	// a second child accumulates.
	child2 := &store.Session{ID: "agent-child2", ParentID: "agent-parent", Status: store.StatusWorking}
	require.NoError(t, st.Insert(ctx, child2))
	s.addChildEdge(ctx, child2)
	require.ElementsMatch(t, []string{"agent-child", "agent-child2"}, childAgents(t, st, "agent-parent"))

	// --- delete: remove edge (both ends) ---
	s.removeChildEdge(ctx, child)
	require.Equal(t, []string{"agent-child2"}, childAgents(t, st, "agent-parent"))
	// removing the last child clears the list entirely (omitempty).
	s.removeChildEdge(ctx, child2)
	require.Nil(t, childAgents(t, st, "agent-parent"))

	// --- reparent: move edge between parents (both ends maintained) ---
	newParent := &store.Session{ID: "agent-parent2", Status: store.StatusWorking}
	require.NoError(t, st.Insert(ctx, newParent))
	// re-establish child under the original parent.
	require.NoError(t, st.Update(ctx, "agent-child", func(c *store.Session) error { c.ParentID = "agent-parent"; return nil }))
	s.addChildEdge(ctx, child)
	assertBothEnds("agent-child", "agent-parent", true)

	// reparent to agent-parent2: old parent loses it, new parent gains it, and the
	// child's back-ref is updated by the caller.
	s.reparentChildEdge(ctx, "agent-child", "agent-parent", "agent-parent2")
	require.NoError(t, st.Update(ctx, "agent-child", func(c *store.Session) error { c.ParentID = "agent-parent2"; return nil }))
	require.Nil(t, childAgents(t, st, "agent-parent"))
	assertBothEnds("agent-child", "agent-parent2", true)
}

// TestChildEdgeExclusions checks childOfParent gating (D5/§6.2, §6.4): job agents,
// terminals, root spawns, and self-parents never populate a ChildAgents[] list.
func TestChildEdgeExclusions(t *testing.T) {
	ctx := context.Background()
	st, err := store.NewFileStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { st.Close(ctx) })
	s := &Server{store: st}

	parent := &store.Session{ID: "agent-parent", Status: store.StatusWorking}
	require.NoError(t, st.Insert(ctx, parent))

	cases := []struct {
		name string
		sess *store.Session
	}{
		{"root spawn (no parent)", &store.Session{ID: "root", Status: store.StatusWorking}},
		{"job agent (pipeline)", &store.Session{ID: "job", ParentID: "agent-parent", PipelineID: "pipe-1", Status: store.StatusWorking}},
		{"job agent (job id)", &store.Session{ID: "job2", ParentID: "agent-parent", JobID: "j1", Status: store.StatusWorking}},
		{"terminal", &store.Session{ID: "term", ParentID: "agent-parent", Kind: store.KindTerminal, Status: store.StatusWorking}},
		{"self parent", &store.Session{ID: "agent-parent", ParentID: "agent-parent", Status: store.StatusWorking}},
	}
	for _, tc := range cases {
		require.False(t, childOfParent(tc.sess), tc.name)
		s.addChildEdge(ctx, tc.sess) // must be a no-op
	}
	require.Nil(t, childAgents(t, st, "agent-parent"), "no excluded session should populate the forward edge")
}

// TestChildEdgeRejectsTerminalParent enforces §6.4 leaf ownership: a terminal
// never owns children. Even when a child carries ParentID pointing at a
// terminal, addChildEdge / reparentChildEdge must not write ChildAgents[].
func TestChildEdgeRejectsTerminalParent(t *testing.T) {
	ctx := context.Background()
	st, err := store.NewFileStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { st.Close(ctx) })
	s := &Server{store: st}

	term := &store.Session{ID: "term-parent", Kind: store.KindTerminal, Status: store.StatusWorking}
	require.NoError(t, st.Insert(ctx, term))

	child := &store.Session{ID: "agent-child", ParentID: "term-parent", Status: store.StatusWorking}
	require.NoError(t, st.Insert(ctx, child))
	s.addChildEdge(ctx, child)
	require.Nil(t, childAgents(t, st, "term-parent"), "terminal must not gain ChildAgents[]")
	gotChild, err := st.Get(ctx, "agent-child")
	require.NoError(t, err)
	require.Empty(t, gotChild.ParentID, "terminal parent back-ref must be cleared on the child")
	require.Empty(t, child.ParentID, "in-memory child ParentID cleared too")

	// reparent attach onto a terminal is likewise rejected.
	agentParent := &store.Session{ID: "agent-parent", Status: store.StatusWorking}
	require.NoError(t, st.Insert(ctx, agentParent))
	child.ParentID = "agent-parent"
	require.NoError(t, st.Update(ctx, "agent-child", func(c *store.Session) error { c.ParentID = "agent-parent"; return nil }))
	s.addChildEdge(ctx, child)
	require.Equal(t, []string{"agent-child"}, childAgents(t, st, "agent-parent"))

	s.reparentChildEdge(ctx, "agent-child", "agent-parent", "term-parent")
	require.Nil(t, childAgents(t, st, "agent-parent"), "detach from old parent still applies")
	require.Nil(t, childAgents(t, st, "term-parent"), "attach to terminal parent must be rejected")
	gotChild, err = st.Get(ctx, "agent-child")
	require.NoError(t, err)
	require.Empty(t, gotChild.ParentID, "reparent to terminal clears child back-ref")
}

// TestChildEdgeDanglingParent tolerates a missing parent record (§6.3): the add
// is a logged no-op, never a fatal error.
func TestChildEdgeDanglingParent(t *testing.T) {
	ctx := context.Background()
	st, err := store.NewFileStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { st.Close(ctx) })
	s := &Server{store: st}

	child := &store.Session{ID: "agent-child", ParentID: "ghost-parent", Status: store.StatusWorking}
	require.NoError(t, st.Insert(ctx, child))
	require.NotPanics(t, func() {
		s.addChildEdge(ctx, child)
		s.removeChildEdge(ctx, child)
		s.reparentChildEdge(ctx, "agent-child", "ghost-parent", "also-ghost")
	})
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
