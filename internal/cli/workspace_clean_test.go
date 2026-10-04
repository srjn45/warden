package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func cleanGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
	c.Dir = dir
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// cleanRepo returns a repo with one merged branch feat/done (local only).
func cleanRepo(t *testing.T, withMerged bool) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	cleanGit(t, repo, "init", "-b", "main")
	_ = os.WriteFile(filepath.Join(repo, "a"), []byte("a"), 0o644)
	cleanGit(t, repo, "add", ".")
	cleanGit(t, repo, "commit", "-m", "init")
	if withMerged {
		cleanGit(t, repo, "checkout", "-b", "feat/done")
		_ = os.WriteFile(filepath.Join(repo, "b"), []byte("b"), 0o644)
		cleanGit(t, repo, "add", ".")
		cleanGit(t, repo, "commit", "-m", "b")
		cleanGit(t, repo, "checkout", "main")
		cleanGit(t, repo, "merge", "--no-ff", "-m", "m", "feat/done")
	}
	return repo
}

func runClean2(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(append(args, "--local-only", "--config", t.TempDir()+"/none.yaml"))
	err := root.Execute()
	return out.String(), err
}

func hasBranch(t *testing.T, repo, b string) bool {
	return strings.TrimSpace(cleanGit(t, repo, "branch", "--list", b)) != ""
}

func TestWorkspaceCleanDryRunAndAlias(t *testing.T) {
	repo := cleanRepo(t, true)
	for _, args := range [][]string{{"workspace", "clean"}, {"clean"}} {
		out, err := runClean2(t, "", append(args, "--dry-run", "--repo", repo)...)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "Merged Local Branches") || !strings.Contains(out, "feat/done") || !strings.Contains(out, "Would reclaim") {
			t.Fatalf("out=%q", out)
		}
		if !hasBranch(t, repo, "feat/done") {
			t.Fatal("dry-run mutated")
		}
	}
}

func TestWorkspaceCleanJSON(t *testing.T) {
	repo := cleanRepo(t, true)
	out, err := runClean2(t, "", "clean", "--dry-run", "--json", "--repo", repo)
	if err != nil {
		t.Fatal(err)
	}
	var rep struct {
		Base          string
		LocalBranches []struct{ Name string }
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil || rep.Base != "main" || len(rep.LocalBranches) != 1 {
		t.Fatalf("%v out=%q", err, out)
	}
	out, err = runClean2(t, "", "clean", "--json", "--yes", "--repo", repo)
	if err != nil {
		t.Fatal(err)
	}
	var res struct{ LocalsDeleted []string }
	if err := json.Unmarshal([]byte(out), &res); err != nil || len(res.LocalsDeleted) != 1 || hasBranch(t, repo, "feat/done") {
		t.Fatalf("%v out=%q", err, out)
	}
}

func TestWorkspaceCleanConfirmation(t *testing.T) {
	repo := cleanRepo(t, true)
	out, err := runClean2(t, "n\n", "clean", "--repo", repo)
	if err != nil || !strings.Contains(out, "aborted") || !hasBranch(t, repo, "feat/done") {
		t.Fatalf("err=%v out=%q", err, out)
	}
	out, err = runClean2(t, "y\n", "clean", "--repo", repo)
	if err != nil || !strings.Contains(out, "Reclaim 0 worktrees, delete 1 local branches") || hasBranch(t, repo, "feat/done") {
		t.Fatalf("err=%v out=%q", err, out)
	}
}

func TestWorkspaceCleanNothingToClean(t *testing.T) {
	repo := cleanRepo(t, false)
	out, err := runClean2(t, "", "clean", "--repo", repo)
	if err != nil || !strings.Contains(out, "nothing to clean") || strings.Contains(out, "[y/N]") {
		t.Fatalf("err=%v out=%q", err, out)
	}
}
