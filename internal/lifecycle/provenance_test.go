package lifecycle

import (
	"context"
	"testing"

	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// TestSpawnDevelopmentRecordsProvenance: a development worktree on a new branch
// is both worktree- and branch-created.
func TestSpawnDevelopmentRecordsProvenance(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"git worktree list --porcelain": {Out: noOtherWorktrees},
	}}
	s, err := New(fr, &FakeConfig{}).Spawn(context.Background(), SpawnRequest{
		Type: store.TypeDevelopment, Ticket: "PROJ-350", Repo: "/repo",
	})
	require.NoError(t, err)
	require.True(t, s.WorktreeCreated, "warden ran `git worktree add`")
	require.True(t, s.BranchCreated, "warden created the branch")
}

// TestSpawnAdoptedRecordsProvenance: adopting a pre-existing worktree records
// neither flag, so later teardown leaves the user's worktree/branch alone.
func TestSpawnAdoptedRecordsProvenance(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"git worktree list --porcelain": {Out: noOtherWorktrees + "\nworktree /repo/.worktrees/PROJ-350\nHEAD def\nbranch refs/heads/PROJ-350\n"},
	}}
	s, err := New(fr, &FakeConfig{}).Spawn(context.Background(), SpawnRequest{
		Type: store.TypeDevelopment, Ticket: "PROJ-350", Repo: "/repo",
	})
	require.NoError(t, err)
	require.False(t, s.WorktreeCreated, "adopted worktree is not warden-created")
	require.False(t, s.BranchCreated, "adopted branch is not warden-created")
}

// TestSpawnPRReviewDetachedCapturesBranch: a detached pr-review captures the
// local branch `gh pr checkout` created (rev-parse names it) and records it as
// warden-owned, so teardown deletes it (no leak).
func TestSpawnPRReviewDetachedCapturesBranch(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"git worktree list --porcelain":   {Out: noOtherWorktrees},
		"git rev-parse --abbrev-ref HEAD": {Out: "pr-12345-fix\n"},
	}}
	s, err := New(fr, &FakeConfig{}).Spawn(context.Background(), SpawnRequest{
		Type: store.TypePRReview, Repo: "/repo", PR: "12345",
	})
	require.NoError(t, err)
	require.True(t, s.WorktreeCreated)
	require.Equal(t, "pr-12345-fix", s.Branch, "gh-created branch captured")
	require.True(t, s.BranchCreated, "gh made the branch, so warden owns its deletion")
}

// TestSpawnPRReviewDetachedFallback: when rev-parse fails or returns HEAD (still
// detached), the capture falls back to Branch="" / BranchCreated=false and prune
// sweeps any dangling branch later.
func TestSpawnPRReviewDetachedFallback(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"git worktree list --porcelain":   {Out: noOtherWorktrees},
		"git rev-parse --abbrev-ref HEAD": {Out: "HEAD\n"},
	}}
	s, err := New(fr, &FakeConfig{}).Spawn(context.Background(), SpawnRequest{
		Type: store.TypePRReview, Repo: "/repo", PR: "12345",
	})
	require.NoError(t, err)
	require.True(t, s.WorktreeCreated)
	require.Empty(t, s.Branch)
	require.False(t, s.BranchCreated, "still detached → no branch to own")
}

// TestSpawnPRReviewExistingBranchProvenance: checking out a user-named branch
// creates the worktree but adopts the branch.
func TestSpawnPRReviewExistingBranchProvenance(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"git worktree list --porcelain": {Out: noOtherWorktrees},
	}}
	s, err := New(fr, &FakeConfig{}).Spawn(context.Background(), SpawnRequest{
		Type: store.TypePRReview, Repo: "/repo", Branch: "feature/login",
	})
	require.NoError(t, err)
	require.True(t, s.WorktreeCreated)
	require.Equal(t, "feature/login", s.Branch)
	require.False(t, s.BranchCreated, "an existing checked-out branch is adopted")
}
