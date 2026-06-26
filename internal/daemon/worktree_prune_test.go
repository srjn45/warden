package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// retentionServer builds a Server wired with the worktree retention policy.
func retentionServer(t *testing.T, fs *fakeStore, fl *fakeLife, keepDone, autoPrune bool) *httptest.Server {
	t.Helper()
	srv := &Server{store: fs, life: fl, hub: newHub(), done: make(chan struct{})}
	srv.SetWorktreeRetention(keepDone, autoPrune)
	return httptest.NewServer(srv.router())
}

// worktree_keep_done=false: archiving a clean-worktree session triggers a
// guarded (force=false) RemoveWorktree.
func TestKeepDoneFalse_RemovesCleanWorktreeOnArchive(t *testing.T) {
	fs := newFakeStore()
	_ = fs.Insert(context.Background(), &store.Session{
		ID: "A-1", TmuxSession: "A-1", Repo: "/repo",
		Worktree: "/repo/.worktrees/A-1", Branch: "A-1", BranchCreated: true,
		Status: store.StatusDone,
	})
	fl := &fakeLife{} // RemoveWorktree succeeds (clean)
	ts := retentionServer(t, fs, fl, false, false)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/v1/sessions/A-1/delete", "application/json", strings.NewReader(`{}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	_, err = fs.Get(context.Background(), "A-1")
	require.ErrorIs(t, err, store.ErrNotFound, "record archived out of the active store")
	require.Equal(t, "A-1", fl.removedWT, "clean worktree removed on archive")
	require.False(t, fl.removeWTForce, "removal must stay guarded (force=false)")
}

// worktree_keep_done=false: a dirty worktree is kept (guard refuses) but the
// archive still succeeds — the removal must never block teardown.
func TestKeepDoneFalse_KeepsDirtyWorktreeButArchiveSucceeds(t *testing.T) {
	fs := newFakeStore()
	_ = fs.Insert(context.Background(), &store.Session{
		ID: "A-1", TmuxSession: "A-1", Repo: "/repo",
		Worktree: "/repo/.worktrees/A-1", Branch: "A-1", BranchCreated: true,
		Status: store.StatusDone,
	})
	fl := &fakeLife{removeWTErr: lifecycle.ErrDirtyWorktree} // guard refuses
	ts := retentionServer(t, fs, fl, false, false)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/v1/sessions/A-1/delete", "application/json", strings.NewReader(`{}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "archive succeeds despite the guard refusal")

	_, err = fs.Get(context.Background(), "A-1")
	require.ErrorIs(t, err, store.ErrNotFound, "record still archived")
}

// Default (worktree_keep_done=true): archiving leaves the worktree untouched.
func TestKeepDoneTrue_LeavesWorktreeOnArchive(t *testing.T) {
	fs := newFakeStore()
	_ = fs.Insert(context.Background(), &store.Session{
		ID: "A-1", TmuxSession: "A-1", Repo: "/repo",
		Worktree: "/repo/.worktrees/A-1", Status: store.StatusDone,
	})
	fl := &fakeLife{}
	ts := retentionServer(t, fs, fl, true, false)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/v1/sessions/A-1/delete", "application/json", strings.NewReader(`{}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Empty(t, fl.removedWT, "keep_done=true must not remove the worktree")
}

// A hard delete never triggers the keep_done removal — it leaves a record-less
// orphan for `warden prune` to reclaim instead.
func TestKeepDoneFalse_HardDeleteSkipsRemoval(t *testing.T) {
	fs := newFakeStore()
	_ = fs.Insert(context.Background(), &store.Session{
		ID: "A-1", TmuxSession: "A-1", Repo: "/repo",
		Worktree: "/repo/.worktrees/A-1", Status: store.StatusDone,
	})
	fl := &fakeLife{}
	ts := retentionServer(t, fs, fl, false, false)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/v1/sessions/A-1/delete", "application/json", strings.NewReader(`{"hard":true}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Empty(t, fl.removedWT, "hard delete must not run the keep_done removal")
}

// worktree_auto_prune: the unattended sweep reconciles each tracked repo with
// Force=false and — the INVARIANT — IncludeArchived=false, so it never touches
// archived-owned worktrees. It still passes the archived records so archived
// owners are recognized and kept (not misclassified as orphans).
func TestAutoPruneSweep_NeverIncludesArchived(t *testing.T) {
	fs := newFakeStore()
	_ = fs.Insert(context.Background(), &store.Session{
		ID: "live-1", Repo: "/repo", Worktree: "/repo/.worktrees/live-1", Status: store.StatusWorking,
	})
	// An archived session owning a worktree in the same repo.
	_ = fs.Insert(context.Background(), &store.Session{
		ID: "done-1", Repo: "/repo", Worktree: "/repo/.worktrees/done-1", Status: store.StatusDone,
	})
	_ = fs.Archive(context.Background(), "done-1")

	fl := &fakeLife{}
	srv := &Server{store: fs, life: fl}
	srv.SetWorktreeRetention(true, true) // autoPrune on

	srv.sweepWorktreesOnce(context.Background())

	require.Equal(t, "/repo", fl.pruneRepo)
	require.False(t, fl.pruneOpts.IncludeArchived, "unattended sweep must NEVER include archived-owned worktrees")
	require.False(t, fl.pruneOpts.Force, "unattended sweep never forces past the dirty/unpushed guard")
	require.False(t, fl.pruneOpts.DryRun, "the sweep is a real run")
	require.Len(t, fl.pruneOpts.Active, 1, "active records passed for ownership classification")
	require.Len(t, fl.pruneOpts.Archived, 1, "archived records passed so archived owners are kept, not pruned")
}

// worktreeRepos collects the distinct repos of worktree-owning sessions across
// active + archived, skipping records with no repo or no worktree.
func TestWorktreeRepos_DistinctOwningRepos(t *testing.T) {
	active := []*store.Session{
		{ID: "a", Repo: "/r1", Worktree: "/r1/.worktrees/a"},
		{ID: "b", Repo: "/r1", Worktree: "/r1/.worktrees/b"}, // dup repo
		{ID: "c", Repo: "/r2", Worktree: ""},                 // no worktree → skip
		{ID: "d", Repo: "", Worktree: "/x"},                  // no repo → skip
	}
	archived := []*store.Session{
		{ID: "e", Repo: "/r3", Worktree: "/r3/.worktrees/e"},
		{ID: "f", Repo: "/r1", Worktree: "/r1/.worktrees/f"}, // dup with active
	}
	got := worktreeRepos(active, archived)
	require.ElementsMatch(t, []string{"/r1", "/r3"}, got)
}
