package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestProvenanceRoundTrip verifies the WorktreeCreated/BranchCreated flags
// survive create → read → list unchanged.
func TestProvenanceRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := newFileStore(t)

	s := sample()
	s.WorktreeCreated = true
	s.BranchCreated = true
	require.NoError(t, st.Insert(ctx, s))

	got, err := st.Get(ctx, s.ID)
	require.NoError(t, err)
	require.True(t, got.WorktreeCreated)
	require.True(t, got.BranchCreated)

	list, err := st.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.True(t, list[0].WorktreeCreated)
	require.True(t, list[0].BranchCreated)
}

// TestProvenanceAdoptedRoundTrip verifies a record that adopted a pre-existing
// worktree/branch round-trips with both flags false.
func TestProvenanceAdoptedRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := newFileStore(t)

	s := sample()
	s.WorktreeCreated = false
	s.BranchCreated = false
	require.NoError(t, st.Insert(ctx, s))

	got, err := st.Get(ctx, s.ID)
	require.NoError(t, err)
	require.False(t, got.WorktreeCreated)
	require.False(t, got.BranchCreated)
}

// TestParentIDRoundTrip verifies an agent's ParentID provenance survives
// create → read → list, and that an unset ParentID stays empty (root).
func TestParentIDRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := newFileStore(t)

	child := sample()
	child.ParentID = "orchestrator-1"
	require.NoError(t, st.Insert(ctx, child))

	root := sample()
	root.ID = child.ID + "-root"
	root.TmuxSession = root.ID
	require.NoError(t, st.Insert(ctx, root))

	gotChild, err := st.Get(ctx, child.ID)
	require.NoError(t, err)
	require.Equal(t, "orchestrator-1", gotChild.ParentID)

	gotRoot, err := st.Get(ctx, root.ID)
	require.NoError(t, err)
	require.Empty(t, gotRoot.ParentID, "an operator/CLI spawn has no parent")

	list, err := st.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 2)
}

// TestListClosed verifies archived records are readable via ListClosed and are
// absent from the active List.
func TestListClosed(t *testing.T) {
	ctx := context.Background()
	st := newFileStore(t)

	s := sample()
	s.WorktreeCreated = true
	s.BranchCreated = true
	require.NoError(t, st.Insert(ctx, s))
	require.NoError(t, st.Archive(ctx, s.ID))

	active, err := st.List(ctx)
	require.NoError(t, err)
	require.Empty(t, active, "archived record must leave the active collection")

	closed, err := st.ListClosed(ctx)
	require.NoError(t, err)
	require.Len(t, closed, 1)
	require.Equal(t, s.ID, closed[0].ID)
	require.True(t, closed[0].WorktreeCreated)
	require.True(t, closed[0].BranchCreated)
}

// TestProvenanceMigration verifies the backfill folded into the legacy-JSON
// import: a legacy record whose branch equals its id is warden-created (both
// flags true), while a user-named branch (≠ id) is conservatively adopted
// (branch flag false). The backfill runs over both the active and closed legacy
// dirs when the old .provenance-migrated marker is absent.
func TestProvenanceMigration(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	// Legacy active record: branch == id → warden-created.
	wardenBranch := &Session{
		ID: "dev-aaaa", TmuxSession: "dev-aaaa", Repo: "/repo",
		Worktree: ".worktrees/dev-aaaa", Branch: "dev-aaaa", Status: StatusWorking,
	}
	// Legacy active record: user-named branch ≠ id → adopted branch.
	userBranch := &Session{
		ID: "dev-bbbb", TmuxSession: "dev-bbbb", Repo: "/repo",
		Worktree: ".worktrees/dev-bbbb", Branch: "feature/login", Status: StatusWorking,
	}
	// Legacy archived record: branch == id → warden-created.
	closedBranch := &Session{
		ID: "dev-cccc", TmuxSession: "dev-cccc", Repo: "/repo",
		Worktree: ".worktrees/dev-cccc", Branch: "dev-cccc", Status: StatusDone,
	}
	// Seed the legacy dirs BEFORE the first open, so the import backfills them (no
	// .provenance-migrated marker present).
	writeLegacy(t, dir, "sessions", wardenBranch)
	writeLegacy(t, dir, "sessions", userBranch)
	writeLegacy(t, dir, "closed", closedBranch)

	st, err := NewFileStore(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close(ctx) })

	gotWarden, err := st.Get(ctx, "dev-aaaa")
	require.NoError(t, err)
	require.True(t, gotWarden.WorktreeCreated)
	require.True(t, gotWarden.BranchCreated, "branch == id must backfill BranchCreated=true")

	gotUser, err := st.Get(ctx, "dev-bbbb")
	require.NoError(t, err)
	require.True(t, gotUser.WorktreeCreated)
	require.False(t, gotUser.BranchCreated, "user-named branch != id must stay adopted")

	closed, err := st.ListClosed(ctx)
	require.NoError(t, err)
	require.Len(t, closed, 1)
	require.True(t, closed[0].WorktreeCreated)
	require.True(t, closed[0].BranchCreated, "archived legacy record must also be backfilled")
}

// TestBackfillProvenanceRule unit-tests the pure backfill rule.
func TestBackfillProvenanceRule(t *testing.T) {
	cases := []struct {
		name             string
		worktree, branch string
		id               string
		wantWorktree     bool
		wantBranch       bool
	}{
		{"warden branch equals id", ".worktrees/x", "x", "x", true, true},
		{"user-named branch", ".worktrees/x", "feature/x", "x", true, false},
		{"no branch", ".worktrees/x", "", "x", true, false},
		{"no worktree", "", "", "x", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Session{ID: tc.id, Worktree: tc.worktree, Branch: tc.branch}
			backfillProvenance(s)
			require.Equal(t, tc.wantWorktree, s.WorktreeCreated)
			require.Equal(t, tc.wantBranch, s.BranchCreated)
		})
	}
}
