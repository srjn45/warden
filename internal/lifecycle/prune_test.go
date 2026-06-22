package lifecycle

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

func TestParseWorktreeList(t *testing.T) {
	out := "" +
		"worktree /repo\nHEAD aaa\nbranch refs/heads/main\n\n" +
		"worktree /repo/.worktrees/A-1\nHEAD bbb\nbranch refs/heads/A-1\n\n" +
		"worktree /repo/.worktrees/pr-9\nHEAD ccc\ndetached\n\n" +
		"worktree /repo/.worktrees/locked-1\nHEAD ddd\nbranch refs/heads/locked-1\nlocked\n\n" +
		"worktree /repo/bare\nbare\n"
	got := parseWorktreeList(out)
	require.Equal(t, []WorktreeInfo{
		{Path: "/repo", Branch: "main"},
		{Path: "/repo/.worktrees/A-1", Branch: "A-1"},
		{Path: "/repo/.worktrees/pr-9", Detached: true},
		{Path: "/repo/.worktrees/locked-1", Branch: "locked-1", Locked: true},
		{Path: "/repo/bare"},
	}, got)
}

func TestWardenWorktreesFilter(t *testing.T) {
	entries := []WorktreeInfo{
		{Path: "/repo", Branch: "main"},               // primary checkout — excluded
		{Path: "/repo/.worktrees", Branch: "x"},       // the dir itself — excluded
		{Path: "/repo/.worktrees/A-1", Branch: "A-1"}, // kept
		{Path: "/other/.worktrees/B", Branch: "B"},    // different repo — excluded
	}
	got := wardenWorktrees("/repo", entries)
	require.Len(t, got, 1)
	require.Equal(t, "/repo/.worktrees/A-1", got[0].Path)
}

// --- prune fakes ---

func mainEntry() string { return "worktree /repo\nHEAD m\nbranch refs/heads/main\n" }

// wtEntry renders one .worktrees/<id> porcelain entry; an empty branch ⇒ detached.
func wtEntry(id, branch string) string {
	tail := "detached\n"
	if branch != "" {
		tail = "branch refs/heads/" + branch + "\n"
	}
	return "worktree /repo/.worktrees/" + id + "\nHEAD h" + id + "\n" + tail
}

func wtSess(id, branch string, branchCreated bool) *store.Session {
	return &store.Session{ID: id, Repo: "/repo", Worktree: ".worktrees/" + id, Branch: branch, BranchCreated: branchCreated}
}

func pruneByPath(res []PruneResult) map[string]PruneResult {
	m := map[string]PruneResult{}
	for _, r := range res {
		m[r.Path] = r
	}
	return m
}

// Live- and archived-owned worktrees are kept (archived even without
// --include-archived); a record-less worktree is an orphan and reclaimed.
func TestPruneClassifiesOwnership(t *testing.T) {
	porcelain := mainEntry() + wtEntry("live-1", "live-1") + wtEntry("arch-1", "arch-1") + wtEntry("orph-1", "orph-1")
	fr := &FakeRunner{Responses: map[string]FakeResp{"git worktree list --porcelain": {Out: porcelain}}}
	res, err := New(fr, &FakeConfig{}).PruneWorktrees(context.Background(), "/repo", PruneOpts{
		Active:   []*store.Session{wtSess("live-1", "live-1", true)},
		Archived: []*store.Session{wtSess("arch-1", "arch-1", true)},
	})
	require.NoError(t, err)
	by := pruneByPath(res)
	require.Equal(t, PruneKeep, by[".worktrees/live-1"].Action)
	require.Equal(t, "live", by[".worktrees/live-1"].Lifecycle)
	require.Equal(t, PruneKeep, by[".worktrees/arch-1"].Action, "archived owner kept without --include-archived")
	require.Equal(t, PruneRemove, by[".worktrees/orph-1"].Action)
	// A clean orphan's worktree is removed, but its branch needs --force.
	require.False(t, by[".worktrees/orph-1"].BranchDeleted)
	require.Contains(t, fr.calledArgs(), []string{"git", "-C", "/repo", "worktree", "remove", ".worktrees/orph-1"})
	require.NotContains(t, fr.calledArgs(), []string{"git", "-C", "/repo", "worktree", "remove", ".worktrees/live-1"})
}

// --include-archived makes an archived-owned worktree eligible; its branch is
// deleted by recorded provenance (BranchCreated), not the orphan name rule.
func TestPruneIncludeArchivedReclaims(t *testing.T) {
	porcelain := mainEntry() + wtEntry("arch-1", "feature/login")
	fr := &FakeRunner{Responses: map[string]FakeResp{"git worktree list --porcelain": {Out: porcelain}}}
	res, err := New(fr, &FakeConfig{}).PruneWorktrees(context.Background(), "/repo", PruneOpts{
		IncludeArchived: true,
		Archived:        []*store.Session{wtSess("arch-1", "feature/login", true)}, // warden-created branch
	})
	require.NoError(t, err)
	require.Equal(t, PruneRemove, res[0].Action)
	require.True(t, res[0].BranchDeleted, "archived owner honors recorded BranchCreated")
	require.Contains(t, fr.calledArgs(), []string{"git", "-C", "/repo", "branch", "-D", "feature/login"})
}

// An archived-owned worktree on an adopted branch (BranchCreated=false) is
// removed under --include-archived but its branch is left alone.
func TestPruneIncludeArchivedKeepsAdoptedBranch(t *testing.T) {
	porcelain := mainEntry() + wtEntry("arch-1", "feature/login")
	fr := &FakeRunner{Responses: map[string]FakeResp{"git worktree list --porcelain": {Out: porcelain}}}
	res, err := New(fr, &FakeConfig{}).PruneWorktrees(context.Background(), "/repo", PruneOpts{
		IncludeArchived: true,
		Archived:        []*store.Session{wtSess("arch-1", "feature/login", false)},
	})
	require.NoError(t, err)
	require.Equal(t, PruneRemove, res[0].Action)
	require.False(t, res[0].BranchDeleted)
	require.NotContains(t, fr.calledArgs(), []string{"git", "-C", "/repo", "branch", "-D", "feature/login"})
}

// A clean orphan removes worktree + branch under --force (branch == id).
func TestPruneCleanOrphanRemovesBranchUnderForce(t *testing.T) {
	porcelain := mainEntry() + wtEntry("orph-1", "orph-1")
	fr := &FakeRunner{Responses: map[string]FakeResp{"git worktree list --porcelain": {Out: porcelain}}}
	res, err := New(fr, &FakeConfig{}).PruneWorktrees(context.Background(), "/repo", PruneOpts{Force: true})
	require.NoError(t, err)
	require.Equal(t, PruneRemove, res[0].Action)
	require.True(t, res[0].BranchDeleted)
	require.Contains(t, fr.calledArgs(), []string{"git", "-C", "/repo", "worktree", "remove", "--force", ".worktrees/orph-1"})
	require.Contains(t, fr.calledArgs(), []string{"git", "-C", "/repo", "branch", "-D", "orph-1"})
}

// The repo's default branch is never branch -D'd, even under --force.
func TestPruneNeverDeletesDefaultBranch(t *testing.T) {
	// An orphan worktree dir literally named "main", checked out on main.
	porcelain := mainEntry() + wtEntry("main", "main")
	fr := &FakeRunner{Responses: map[string]FakeResp{"git worktree list --porcelain": {Out: porcelain}}}
	res, err := New(fr, &FakeConfig{}).PruneWorktrees(context.Background(), "/repo", PruneOpts{Force: true})
	require.NoError(t, err)
	require.Equal(t, PruneRemove, res[0].Action, "worktree still removed")
	require.False(t, res[0].BranchDeleted)
	require.NotContains(t, fr.calledArgs(), []string{"git", "-C", "/repo", "branch", "-D", "main"})
}

func TestPruneSkipsDirtyWithoutForce(t *testing.T) {
	porcelain := mainEntry() + wtEntry("orph-1", "orph-1")
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"git worktree list --porcelain":                     {Out: porcelain},
		"git -C /repo/.worktrees/orph-1 status --porcelain": {Out: " M f.go\n"},
	}}
	res, err := New(fr, &FakeConfig{}).PruneWorktrees(context.Background(), "/repo", PruneOpts{})
	require.NoError(t, err)
	require.Equal(t, PruneSkip, res[0].Action)
	require.Equal(t, "dirty", res[0].State)
	require.NotContains(t, fr.calledArgs(), []string{"git", "-C", "/repo", "worktree", "remove", ".worktrees/orph-1"})
}

func TestPruneSkipsUnpushedWithoutForce(t *testing.T) {
	porcelain := mainEntry() + wtEntry("orph-1", "orph-1")
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"git worktree list --porcelain":                       {Out: porcelain},
		"git -C /repo/.worktrees/orph-1 log @{u}.. --oneline": {Err: errStub("no upstream")},
	}}
	res, err := New(fr, &FakeConfig{}).PruneWorktrees(context.Background(), "/repo", PruneOpts{})
	require.NoError(t, err)
	require.Equal(t, PruneSkip, res[0].Action)
	require.Equal(t, "unpushed", res[0].State)
}

func TestPruneRemovesDirtyWithForce(t *testing.T) {
	porcelain := mainEntry() + wtEntry("orph-1", "orph-1")
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"git worktree list --porcelain":                     {Out: porcelain},
		"git -C /repo/.worktrees/orph-1 status --porcelain": {Out: " M f.go\n"},
	}}
	res, err := New(fr, &FakeConfig{}).PruneWorktrees(context.Background(), "/repo", PruneOpts{Force: true})
	require.NoError(t, err)
	require.Equal(t, PruneRemove, res[0].Action)
	require.Contains(t, fr.calledArgs(), []string{"git", "-C", "/repo", "worktree", "remove", "--force", ".worktrees/orph-1"})
}

// The dry-run plan is identical to the real run's results, and a dry-run removes
// nothing and never runs `git worktree prune`; a real run does.
func TestPruneDryRunMatchesRealRun(t *testing.T) {
	porcelain := mainEntry() + wtEntry("live-1", "live-1") + wtEntry("orph-1", "orph-1") + wtEntry("orph-2", "orph-2")
	mk := func() *FakeRunner {
		return &FakeRunner{Responses: map[string]FakeResp{"git worktree list --porcelain": {Out: porcelain}}}
	}
	opts := func(dry bool) PruneOpts {
		return PruneOpts{DryRun: dry, Force: true, Active: []*store.Session{wtSess("live-1", "live-1", true)}}
	}

	frDry := mk()
	dry, err := New(frDry, &FakeConfig{}).PruneWorktrees(context.Background(), "/repo", opts(true))
	require.NoError(t, err)
	frReal := mk()
	real_, err := New(frReal, &FakeConfig{}).PruneWorktrees(context.Background(), "/repo", opts(false))
	require.NoError(t, err)
	require.Equal(t, dry, real_, "dry-run plan must equal the real-run plan")

	for _, a := range frDry.calledArgs() {
		require.NotContains(t, a, "remove", "dry-run must not remove worktrees")
	}
	require.NotContains(t, frDry.calledArgs(), []string{"git", "-C", "/repo", "worktree", "prune"}, "no worktree prune on dry-run")
	require.Contains(t, frReal.calledArgs(), []string{"git", "-C", "/repo", "worktree", "prune"}, "real run runs worktree prune")
}

// Real-git integration: an owned worktree is kept; dropping the record reclaims
// it; an out-of-band removal leaves admin metadata that prune clears so a
// reused-id `worktree add` succeeds.
func TestPruneIntegrationRealGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(repo); err == nil {
		repo = resolved
	}
	git := func(dir string, args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
		return string(out)
	}
	git(repo, "init", "-b", "main")
	git(repo, "config", "user.email", "t@example.com")
	git(repo, "config", "user.name", "t")
	require.NoError(t, os.WriteFile(filepath.Join(repo, "f.txt"), []byte("hi"), 0o644))
	git(repo, "add", ".")
	git(repo, "commit", "-m", "init")

	lc := New(ExecRunner{}, &FakeConfig{})
	ctx := context.Background()

	// spawn-equivalent: a warden worktree on a new branch, with an owning record.
	git(repo, "worktree", "add", ".worktrees/feat-1", "-b", "feat-1")
	owner := &store.Session{ID: "feat-1", Repo: repo, Worktree: ".worktrees/feat-1", Branch: "feat-1", BranchCreated: true}

	// Owned → kept.
	res, err := lc.PruneWorktrees(ctx, repo, PruneOpts{Active: []*store.Session{owner}})
	require.NoError(t, err)
	require.Len(t, res, 1)
	require.Equal(t, PruneKeep, res[0].Action)
	require.DirExists(t, filepath.Join(repo, ".worktrees", "feat-1"))

	// Drop the record → orphan → reclaimed (force also drops the new branch;
	// feat-1 has no upstream so it is "unpushed", which --force overrides).
	res, err = lc.PruneWorktrees(ctx, repo, PruneOpts{Force: true})
	require.NoError(t, err)
	require.Len(t, res, 1)
	require.Equal(t, PruneRemove, res[0].Action)
	require.True(t, res[0].BranchDeleted)
	require.NoDirExists(t, filepath.Join(repo, ".worktrees", "feat-1"))
	_, verr := exec.Command("git", "-C", repo, "rev-parse", "--verify", "feat-1").CombinedOutput()
	require.Error(t, verr, "branch feat-1 should be gone")

	// Out-of-band removal: add a worktree, rm -rf it, then prune must run
	// `git worktree prune` so reusing the id succeeds.
	git(repo, "worktree", "add", ".worktrees/reuse-1", "-b", "reuse-1")
	require.NoError(t, os.RemoveAll(filepath.Join(repo, ".worktrees", "reuse-1")))
	_, err = lc.PruneWorktrees(ctx, repo, PruneOpts{})
	require.NoError(t, err)
	out, err := exec.Command("git", "-C", repo, "worktree", "add", ".worktrees/reuse-1", "reuse-1").CombinedOutput()
	require.NoError(t, err, "reused worktree add must succeed after prune cleared stale admin metadata: %s", out)
}
