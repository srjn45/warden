package daemon

import (
	"context"
	"testing"

	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// A tombstone whose last live child has gone terminal is archived.
func TestReapTombstoneWhenLastChildEnds(t *testing.T) {
	fs := newFakeStore()
	fs.data["parent"] = &store.Session{ID: "parent", Status: store.StatusDone} // tombstoned (terminal, tmux gone)
	fs.data["child"] = &store.Session{ID: "child", ParentID: "parent", Status: store.StatusDone}

	reapTombstones(context.Background(), fs, "parent")

	require.NotContains(t, fs.data, "parent", "tombstone with no live children is reaped")
	require.Contains(t, fs.closed, "parent", "reap uses Archive (retrievable)")
	require.Contains(t, fs.data, "child", "the (now terminal) child record is left in place")
}

// A tombstone that still has a live child is retained.
func TestReapTombstoneRetainedWithLiveChild(t *testing.T) {
	fs := newFakeStore()
	fs.data["parent"] = &store.Session{ID: "parent", Status: store.StatusOrphaned}
	fs.data["dead"] = &store.Session{ID: "dead", ParentID: "parent", Status: store.StatusDone}
	fs.data["alive"] = &store.Session{ID: "alive", ParentID: "parent", Status: store.StatusWorking}

	reapTombstones(context.Background(), fs, "parent")

	require.Contains(t, fs.data, "parent", "a tombstone with ≥1 live child stays put")
}

// A childless terminal agent is an ordinary "done" agent, never auto-reaped.
func TestReapLeavesChildlessTerminalAgent(t *testing.T) {
	fs := newFakeStore()
	fs.data["solo"] = &store.Session{ID: "solo", Status: store.StatusDone}

	reapTombstones(context.Background(), fs, "solo")

	require.Contains(t, fs.data, "solo", "a done agent with no children is left for the operator")
}

// A still-live parent anchors its sub-tree and is not reaped.
func TestReapLeavesLiveParent(t *testing.T) {
	fs := newFakeStore()
	fs.data["parent"] = &store.Session{ID: "parent", Status: store.StatusWorking}
	fs.data["child"] = &store.Session{ID: "child", ParentID: "parent", Status: store.StatusDone}

	reapTombstones(context.Background(), fs, "parent")

	require.Contains(t, fs.data, "parent")
}

// Reaping climbs the chain: a grandparent tombstone is reaped once reaping its
// child (a parent tombstone) leaves it with no live children.
func TestReapClimbsChain(t *testing.T) {
	fs := newFakeStore()
	fs.data["gp"] = &store.Session{ID: "gp", Status: store.StatusDone}
	fs.data["p"] = &store.Session{ID: "p", ParentID: "gp", Status: store.StatusDone}
	fs.data["c"] = &store.Session{ID: "c", ParentID: "p", Status: store.StatusDone}

	// Start from the parent (as the lazy hook would, when child c ended).
	reapTombstones(context.Background(), fs, "p")

	require.NotContains(t, fs.data, "p", "parent tombstone reaped")
	require.NotContains(t, fs.data, "gp", "grandparent reaped once parent archived left it childless-of-live")
	require.Contains(t, fs.data, "c")
}

// The lazy hook on FinalizeExit reaps the parent when a child is finalized.
func TestFinalizeExitReapsParent(t *testing.T) {
	fs := newFakeStore()
	fs.data["parent"] = &store.Session{ID: "parent", Status: store.StatusDone}
	fs.data["child"] = &store.Session{ID: "child", ParentID: "parent", Status: store.StatusWorking}
	d := &pollerDeps{store: fs}

	swapped, err := d.FinalizeExit(context.Background(), "child", store.StatusWorking, store.StatusDone, 0)
	require.NoError(t, err)
	require.True(t, swapped)
	require.NotContains(t, fs.data, "parent", "finalizing the last child reaps the tombstone parent")
}

// The safety-net sweep reaps a reapable tombstone without a transition trigger.
func TestReapAllTombstonesSweep(t *testing.T) {
	fs := newFakeStore()
	fs.data["parent"] = &store.Session{ID: "parent", Status: store.StatusDone}
	fs.data["child"] = &store.Session{ID: "child", ParentID: "parent", Status: store.StatusDone}
	fs.data["solo"] = &store.Session{ID: "solo", Status: store.StatusDone} // childless, must survive
	s := &Server{store: fs}

	s.reapAllTombstones(context.Background())

	require.NotContains(t, fs.data, "parent")
	require.Contains(t, fs.data, "solo", "the sweep never touches a childless done agent")
}
