package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func branchExists(t *testing.T, repo, name string) bool {
	t.Helper()
	return strings.TrimSpace(git(t, repo, "branch", "--list", name)) != ""
}

func remoteHas(t *testing.T, repo, name string) bool {
	t.Helper()
	return strings.TrimSpace(git(t, repo, "ls-remote", "--heads", "origin", name)) != ""
}

func TestApplyFullCleanup(t *testing.T) {
	repo := newRepo(t)
	topic(t, repo, "feat/merge", "m.txt")
	topic(t, repo, "feat/squash", "s.txt")
	topic(t, repo, "feat/live", "l.txt")
	git(t, repo, "merge", "--no-ff", "-m", "m", "feat/merge")
	git(t, repo, "merge", "--squash", "feat/squash")
	git(t, repo, "commit", "-m", "sq")
	git(t, repo, "push", "origin", "main", "feat/merge", "feat/squash")
	wt := filepath.Join(repo, ".worktrees", "merge")
	git(t, repo, "worktree", "add", wt, "feat/merge")

	o := Options{Repo: repo}
	rep := detect(t, o)
	res, err := Apply(context.Background(), o, ApplyOptions{}, rep)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("errors: %v", res.Errors)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Error("worktree not removed")
	}
	if branchExists(t, repo, "feat/merge") || branchExists(t, repo, "feat/squash") {
		t.Error("merged locals remain")
	}
	if !branchExists(t, repo, "feat/live") || !branchExists(t, repo, "main") {
		t.Error("live/main deleted")
	}
	if remoteHas(t, repo, "feat/merge") || remoteHas(t, repo, "feat/squash") {
		t.Error("remotes remain")
	}
	if !remoteHas(t, repo, "main") {
		t.Error("remote main deleted")
	}
	if len(res.WorktreesRemoved) != 1 || len(res.LocalsDeleted) != 2 || len(res.RemotesDeleted) != 2 {
		t.Errorf("result: %+v", res)
	}
}

func TestApplyDryRunMutatesNothing(t *testing.T) {
	repo := newRepo(t)
	topic(t, repo, "feat/a", "a2.txt")
	git(t, repo, "merge", "--no-ff", "-m", "m", "feat/a")
	git(t, repo, "push", "origin", "main", "feat/a")
	wt := filepath.Join(repo, ".worktrees", "a")
	git(t, repo, "worktree", "add", wt, "feat/a")
	o := Options{Repo: repo}
	rep := detect(t, o)
	res, err := Apply(context.Background(), o, ApplyOptions{DryRun: true}, rep)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.WorktreesRemoved) != 1 || len(res.LocalsDeleted) != 1 || len(res.RemotesDeleted) != 1 {
		t.Errorf("would-do: %+v", res)
	}
	if _, err := os.Stat(wt); err != nil || !branchExists(t, repo, "feat/a") || !remoteHas(t, repo, "feat/a") {
		t.Error("dry run mutated")
	}
}

func TestApplyDirtyNeedsForce(t *testing.T) {
	repo := newRepo(t)
	topic(t, repo, "feat/d", "d.txt")
	git(t, repo, "merge", "--no-ff", "-m", "m", "feat/d")
	wt := filepath.Join(repo, ".worktrees", "d")
	git(t, repo, "worktree", "add", wt, "feat/d")
	write(t, wt, "u.txt", "x")
	o := Options{Repo: repo, LocalOnly: true}
	rep := detect(t, o)

	res, _ := Apply(context.Background(), o, ApplyOptions{}, rep)
	if len(res.WorktreesRemoved) != 0 || len(res.LocalsDeleted) != 0 {
		t.Fatalf("dirty removed without force: %+v", res)
	}
	var found bool
	for _, s := range res.Skipped {
		found = found || s.Reason == "dirty (use --force)"
	}
	if !found || !branchExists(t, repo, "feat/d") {
		t.Errorf("skipped=%v", res.Skipped)
	}

	res, err := Apply(context.Background(), o, ApplyOptions{Force: true}, rep)
	if err != nil || len(res.Errors) != 0 {
		t.Fatalf("%v %v", err, res.Errors)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) || branchExists(t, repo, "feat/d") {
		t.Error("force did not clean")
	}
}

func TestApplyRefusesProtected(t *testing.T) {
	repo := newRepo(t)
	topic(t, repo, "release/1", "r.txt")
	git(t, repo, "branch", "develop")
	rep := &Report{Base: "main",
		LocalBranches:  []Branch{{Name: "main", Kind: KindMerged}, {Name: "develop", Kind: KindMerged}, {Name: "release/1", Kind: KindMerged}},
		RemoteBranches: []Branch{{Name: "main", Kind: KindMerged}}}
	res, err := Apply(context.Background(), Options{Repo: repo}, ApplyOptions{}, rep)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.LocalsDeleted)+len(res.RemotesDeleted) != 0 || len(res.Skipped) != 4 {
		t.Errorf("%+v", res)
	}
	if !branchExists(t, repo, "develop") || !remoteHas(t, repo, "main") {
		t.Error("protected deleted")
	}
	// current branch of the primary worktree is protected too
	git(t, repo, "checkout", "-b", "feat/cur")
	res, _ = Apply(context.Background(), Options{Repo: repo}, ApplyOptions{}, &Report{Base: "main", LocalBranches: []Branch{{Name: "feat/cur", Kind: KindMerged}}})
	if len(res.LocalsDeleted) != 0 || !branchExists(t, repo, "feat/cur") {
		t.Errorf("current branch deleted: %+v", res)
	}
}

func TestApplyLocalOnlySkipsRemotes(t *testing.T) {
	repo := newRepo(t)
	topic(t, repo, "feat/l", "l.txt")
	git(t, repo, "merge", "--no-ff", "-m", "m", "feat/l")
	git(t, repo, "push", "origin", "main", "feat/l")
	rep := detect(t, Options{Repo: repo})
	res, _ := Apply(context.Background(), Options{Repo: repo}, ApplyOptions{LocalOnly: true}, rep)
	if len(res.RemotesDeleted) != 0 || !remoteHas(t, repo, "feat/l") || branchExists(t, repo, "feat/l") {
		t.Errorf("%+v", res)
	}
}

func TestApplyUnverifiedSquashNotForceDeleted(t *testing.T) {
	repo := newRepo(t)
	topic(t, repo, "feat/fake", "f.txt") // not merged at all
	rep := &Report{Base: "main", LocalBranches: []Branch{{Name: "feat/fake", Kind: KindSquashMerged, Reason: "x"}}}
	res, _ := Apply(context.Background(), Options{Repo: repo}, ApplyOptions{}, rep)
	if len(res.LocalsDeleted) != 0 || !branchExists(t, repo, "feat/fake") {
		t.Errorf("%+v", res)
	}
}

func TestApplyErrorsDoNotAbort(t *testing.T) {
	repo := newRepo(t)
	topic(t, repo, "feat/ok", "o.txt")
	git(t, repo, "merge", "--no-ff", "-m", "m", "feat/ok")
	rep := &Report{Base: "main", LocalBranches: []Branch{{Name: "feat/missing", Kind: KindMerged}, {Name: "feat/ok", Kind: KindMerged}}}
	res, _ := Apply(context.Background(), Options{Repo: repo}, ApplyOptions{}, rep)
	if len(res.Errors) != 1 || len(res.LocalsDeleted) != 1 || branchExists(t, repo, "feat/ok") {
		t.Errorf("%+v", res)
	}
}
