package store

import (
	"context"
	"os"
	"path/filepath"
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

// TestProvenanceMigration verifies the one-shot backfill applied at store-open:
// a legacy record whose branch equals its id is warden-created (both flags
// true), while a user-named branch (≠ id) is conservatively adopted (branch
// flag false). The migration runs over both active and closed collections.
func TestProvenanceMigration(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	// First open creates the layout; close it so we can drop legacy files in
	// before the migration runs on the next open.
	st0, err := NewFileStore(dir)
	require.NoError(t, err)
	require.NoError(t, st0.Close(ctx))

	// Remove the marker the empty-corpus migration wrote, so re-opening with
	// legacy files actually runs the backfill.
	require.NoError(t, os.Remove(filepath.Join(dir, provenanceMarker)))

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
	require.NoError(t, atomicWriteJSON(filepath.Join(dir, "sessions", "dev-aaaa.json"), wardenBranch))
	require.NoError(t, atomicWriteJSON(filepath.Join(dir, "sessions", "dev-bbbb.json"), userBranch))
	require.NoError(t, atomicWriteJSON(filepath.Join(dir, "closed", "dev-cccc.json"), closedBranch))

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
