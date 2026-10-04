package workspace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRunner runs real git but fakes gh.
type fakeRunner struct {
	gh    string
	ghErr error
	calls []string
}

func (f *fakeRunner) Run(ctx context.Context, dir, name string, args ...string) (string, error) {
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	if name == "gh" {
		return f.gh, f.ghErr
	}
	return ExecRunner{}.Run(ctx, dir, name, args...)
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newRepo makes origin (bare) + a clone on main with one commit.
func newRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	bare := filepath.Join(root, "origin.git")
	repo := filepath.Join(root, "repo")
	git(t, root, "init", "--bare", "-b", "main", bare)
	git(t, root, "init", "-b", "main", repo)
	git(t, repo, "remote", "add", "origin", bare)
	write(t, repo, "a.txt", "a\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "init")
	git(t, repo, "push", "-u", "origin", "main")
	return repo
}

func topic(t *testing.T, repo, name, file string) {
	t.Helper()
	git(t, repo, "checkout", "-b", name)
	write(t, repo, file, name+"\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", name+" one")
	write(t, repo, file, name+"\nmore\n")
	git(t, repo, "commit", "-am", name+" two")
	git(t, repo, "checkout", "main")
}

func names(bs []Branch) map[string]string {
	m := map[string]string{}
	for _, b := range bs {
		m[b.Name] = b.Kind
	}
	return m
}

func detect(t *testing.T, o Options) *Report {
	t.Helper()
	if o.Runner == nil {
		o.Runner = &fakeRunner{ghErr: errors.New("gh: not found")}
	}
	rep, err := Detect(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func TestStandardMerge(t *testing.T) {
	repo := newRepo(t)
	topic(t, repo, "feat/merged", "m.txt")
	topic(t, repo, "feat/open", "o.txt")
	git(t, repo, "merge", "--no-ff", "-m", "merge", "feat/merged")
	git(t, repo, "push", "origin", "main", "feat/merged", "feat/open")

	rep := detect(t, Options{Repo: repo})
	if rep.Base != "main" {
		t.Fatalf("base %q", rep.Base)
	}
	l := names(rep.LocalBranches)
	if l["feat/merged"] != KindMerged || len(l) != 1 {
		t.Fatalf("locals: %v", l)
	}
	r := names(rep.RemoteBranches)
	if r["feat/merged"] != KindMerged || len(r) != 1 {
		t.Fatalf("remotes: %v", r)
	}
}

func TestSquashMergeFallback(t *testing.T) {
	repo := newRepo(t)
	topic(t, repo, "feat/squashed", "s.txt")
	git(t, repo, "merge", "--squash", "feat/squashed")
	git(t, repo, "commit", "-m", "squash")
	git(t, repo, "push", "origin", "main", "feat/squashed")

	if strings.Contains(git(t, repo, "branch", "--merged", "main"), "feat/squashed") {
		t.Fatal("precondition: squashed branch must not be in --merged")
	}
	rep := detect(t, Options{Repo: repo})
	if names(rep.LocalBranches)["feat/squashed"] != KindSquashMerged {
		t.Fatalf("locals: %+v", rep.LocalBranches)
	}
	if names(rep.RemoteBranches)["feat/squashed"] != KindSquashMerged {
		t.Fatalf("remotes: %+v", rep.RemoteBranches)
	}
	var sawGH bool
	for _, s := range rep.Skipped {
		sawGH = sawGH || s.Name == "gh"
	}
	if !sawGH {
		t.Fatal("expected gh-unavailable note in Skipped")
	}
}

func TestGHPath(t *testing.T) {
	repo := newRepo(t)
	topic(t, repo, "feat/squashed", "s.txt")
	// main moved on in a way that does not contain the topic's content.
	rep := detect(t, Options{Repo: repo, Runner: &fakeRunner{
		gh: `[{"headRefName":"feat/squashed","number":7,"title":"Add s"}]`}})
	got := rep.LocalBranches
	if len(got) != 1 || got[0].Kind != KindSquashMerged || !strings.Contains(got[0].Reason, "#7") {
		t.Fatalf("got %+v", got)
	}
}

func TestGHFailureFallsBack(t *testing.T) {
	repo := newRepo(t)
	topic(t, repo, "feat/squashed", "s.txt")
	git(t, repo, "merge", "--squash", "feat/squashed")
	git(t, repo, "commit", "-m", "squash")
	rep := detect(t, Options{Repo: repo, Runner: &fakeRunner{gh: "garbage"}})
	if names(rep.LocalBranches)["feat/squashed"] != KindSquashMerged {
		t.Fatalf("%+v", rep.LocalBranches)
	}
}

func TestProtectedAndActive(t *testing.T) {
	repo := newRepo(t)
	for _, b := range []string{"develop", "release/1.0", "master", "feat/cur", "feat/other"} {
		git(t, repo, "branch", b) // all at base → trivially merged
	}
	git(t, repo, "checkout", "feat/cur")
	git(t, repo, "push", "origin", "develop", "release/1.0", "feat/other")

	rep := detect(t, Options{Repo: repo})
	for _, set := range []map[string]string{names(rep.LocalBranches), names(rep.RemoteBranches)} {
		for _, p := range []string{"main", "master", "develop", "release/1.0", "feat/cur"} {
			if _, ok := set[p]; ok {
				t.Fatalf("protected %s offered: %v", p, set)
			}
		}
	}
	if _, ok := names(rep.LocalBranches)["feat/other"]; !ok {
		t.Fatalf("feat/other should be candidate: %+v", rep.LocalBranches)
	}
	prot := map[string]bool{}
	for _, s := range rep.Skipped {
		if s.Reason == "protected" {
			prot[s.Name] = true
		}
	}
	for _, p := range []string{"develop", "release/1.0", "feat/cur", "origin/develop"} {
		if !prot[p] {
			t.Errorf("%s not in Skipped as protected: %v", p, rep.Skipped)
		}
	}
}

func TestStaleWorktreeDirtyAndClean(t *testing.T) {
	repo := newRepo(t)
	topic(t, repo, "feat/dirty", "d.txt")
	topic(t, repo, "feat/clean", "c.txt")
	topic(t, repo, "feat/live", "l.txt")
	git(t, repo, "merge", "--squash", "feat/dirty")
	git(t, repo, "commit", "-m", "sq dirty")
	git(t, repo, "merge", "--no-ff", "-m", "m", "feat/clean")

	wt := filepath.Join(repo, ".worktrees")
	git(t, repo, "worktree", "add", filepath.Join(wt, "dirty"), "feat/dirty")
	git(t, repo, "worktree", "add", filepath.Join(wt, "clean"), "feat/clean")
	git(t, repo, "worktree", "add", filepath.Join(wt, "live"), "feat/live")
	git(t, repo, "worktree", "add", "--detach", filepath.Join(wt, "det"), "HEAD")
	write(t, filepath.Join(wt, "dirty"), "untracked.txt", "x")

	rep := detect(t, Options{Repo: repo})
	byBranch := map[string]Worktree{}
	for _, w := range rep.StaleWorktrees {
		byBranch[w.Branch] = w
	}
	if len(byBranch) != 2 {
		t.Fatalf("worktrees: %+v", rep.StaleWorktrees)
	}
	if !byBranch["feat/dirty"].Dirty || byBranch["feat/dirty"].Kind != KindSquashMerged {
		t.Errorf("dirty: %+v", byBranch["feat/dirty"])
	}
	if byBranch["feat/clean"].Dirty || byBranch["feat/clean"].Kind != KindMerged {
		t.Errorf("clean: %+v", byBranch["feat/clean"])
	}
	var det bool
	for _, s := range rep.Skipped {
		det = det || (s.Reason == "detached HEAD" && strings.HasSuffix(s.Name, "det"))
	}
	if !det {
		t.Errorf("detached worktree not skipped: %v", rep.Skipped)
	}
}

func TestRemoteOnlyMerged(t *testing.T) {
	repo := newRepo(t)
	topic(t, repo, "fix/remote", "r.txt")
	git(t, repo, "merge", "--no-ff", "-m", "m", "fix/remote")
	git(t, repo, "push", "origin", "main", "fix/remote")
	git(t, repo, "branch", "-D", "fix/remote")

	rep := detect(t, Options{Repo: repo})
	if len(rep.LocalBranches) != 0 || names(rep.RemoteBranches)["fix/remote"] != KindMerged {
		t.Fatalf("%+v / %+v", rep.LocalBranches, rep.RemoteBranches)
	}
}

func TestLocalOnly(t *testing.T) {
	repo := newRepo(t)
	topic(t, repo, "fix/x", "x.txt")
	git(t, repo, "merge", "--no-ff", "-m", "m", "fix/x")
	git(t, repo, "push", "origin", "main", "fix/x")
	rep := detect(t, Options{Repo: repo, LocalOnly: true})
	if len(rep.RemoteBranches) != 0 || len(rep.LocalBranches) != 1 {
		t.Fatalf("%+v / %+v", rep.LocalBranches, rep.RemoteBranches)
	}
}

func TestFetchPrunesAndErrors(t *testing.T) {
	repo := newRepo(t)
	topic(t, repo, "fix/gone", "g.txt")
	git(t, repo, "merge", "--no-ff", "-m", "m", "fix/gone")
	git(t, repo, "push", "origin", "main", "fix/gone")
	// Delete the branch on origin behind the clone's back.
	git(t, repo, "push", "origin", "--delete", "fix/gone")
	// Stale tracking ref still present until fetch --prune.
	git(t, repo, "update-ref", "refs/remotes/origin/fix/gone", "HEAD")

	if rep := detect(t, Options{Repo: repo}); names(rep.RemoteBranches)["fix/gone"] == "" {
		t.Fatal("without Fetch the stale ref should still show")
	}
	if rep := detect(t, Options{Repo: repo, Fetch: true}); len(rep.RemoteBranches) != 0 {
		t.Fatalf("Fetch should prune: %+v", rep.RemoteBranches)
	}

	git(t, repo, "remote", "set-url", "origin", filepath.Join(repo, "nope"))
	_, err := Detect(context.Background(), Options{Repo: repo, Fetch: true, Runner: &fakeRunner{ghErr: errors.New("x")}})
	if err == nil {
		t.Fatal("fetch failure must be an error")
	}
}

func TestMasterDefault(t *testing.T) {
	root := t.TempDir()
	git(t, root, "init", "-b", "master", "r")
	repo := filepath.Join(root, "r")
	write(t, repo, "a", "a")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "i")
	rep := detect(t, Options{Repo: repo})
	if rep.Base != "master" {
		t.Fatalf("base %q", rep.Base)
	}
}

func TestDetectDoesNotMutate(t *testing.T) {
	repo := newRepo(t)
	topic(t, repo, "feat/m", "m.txt")
	git(t, repo, "merge", "--no-ff", "-m", "m", "feat/m")
	before := git(t, repo, "branch", "-a")
	detect(t, Options{Repo: repo})
	if after := git(t, repo, "branch", "-a"); after != before {
		t.Fatalf("branches changed:\n%s\n%s", before, after)
	}
}
