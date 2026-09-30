package planexport

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/planstore"
)

// TestSync_E2ERealGitFakeGitHub exercises the full sync workflow against a real
// git repository fixture with a fake `gh` on PATH. The operator worktree stays
// dirty with an unrelated file that must never be committed.
func TestSync_E2ERealGitFakeGitHub(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	root := t.TempDir()
	bare := filepath.Join(root, "remote.git")
	repo := filepath.Join(root, "repo")
	require.NoError(t, exec.Command("git", "init", "--bare", bare).Run())
	git(t, root, "clone", bare, repo)
	git(t, repo, "config", "user.email", "sync@test.local")
	git(t, repo, "config", "user.name", "Plan Sync Test")
	git(t, repo, "checkout", "-b", "main")
	require.NoError(t, os.WriteFile(filepath.Join(repo, "README.md"), []byte("fixture\n"), 0o644))
	git(t, repo, "add", "README.md")
	git(t, repo, "commit", "-m", "init")
	git(t, repo, "push", "-u", "origin", "main")

	// Dirty unrelated operator change — must remain untouched.
	dirty := filepath.Join(repo, "operator-wip.txt")
	require.NoError(t, os.WriteFile(dirty, []byte("do not commit me\n"), 0o644))
	beforeDirty, err := os.ReadFile(dirty)
	require.NoError(t, err)
	beforeStatus := gitOut(t, repo, "status", "--porcelain")

	ghBin := writeFakeGH(t, root)
	pathEnv := ghBin + string(os.PathListSeparator) + os.Getenv("PATH")
	t.Setenv("PATH", pathEnv)

	plan := &planstore.Plan{
		ID:          "plan-e2e0001",
		ProjectID:   repo,
		Name:        "E2E Sync",
		Goal:        "prove isolated export",
		Status:      planstore.PlanStatusPending,
		Revision:    1,
		Tasks:       []planstore.PlanTask{{ID: "t1", Prompt: "export me"}},
		Constraints: []string{"never touch operator WIP"},
		DoneWhen:    []string{"PR opened"},
	}
	plan.ContentHash = planstore.ComputeContentHash(plan)

	plansDir := filepath.Join(root, "plans-data")
	planDB, err := planstore.New(plansDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = planDB.Close() })
	require.NoError(t, planDB.Create(context.Background(), plan))

	exportDB, err := NewStore(plansDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = exportDB.Close() })

	syncer := &Syncer{
		Plans:    planDB,
		Exports:  exportDB,
		PlansMut: planDB,
		Git:      NewRunnerGitHost(lifecycle.ExecRunner{}),
		Renderer: Default(),
		Now:      func() time.Time { return time.Date(2026, 9, 30, 15, 0, 0, 0, time.UTC) },
	}

	res, err := syncer.Sync(context.Background(), SyncOptions{
		PlanID:    plan.ID,
		RepoPath:  repo,
		TargetRef: "main",
	})
	require.NoError(t, err)
	require.Equal(t, OutcomeSuccess, res.Outcome)
	require.NotEmpty(t, res.CommitSHA)
	require.Equal(t, "https://example.test/pull/1", res.PRURL)
	require.Equal(t, SyncBranch(plan.ID, plan.Revision), res.Branch)
	require.Equal(t, "plans/pending/e2e-sync.yaml", res.OutputPath)

	// Operator dirty file and status unchanged aside from worktree metadata noise;
	// the dirty file content must be identical and still untracked/modified.
	afterDirty, err := os.ReadFile(dirty)
	require.NoError(t, err)
	require.Equal(t, beforeDirty, afterDirty)
	afterStatus := gitOut(t, repo, "status", "--porcelain")
	require.Contains(t, afterStatus, "operator-wip.txt")
	require.Contains(t, beforeStatus, "operator-wip.txt")

	// Export landed only on the dedicated branch, not on main / operator HEAD.
	headBranch := strings.TrimSpace(gitOut(t, repo, "rev-parse", "--abbrev-ref", "HEAD"))
	require.Equal(t, "main", headBranch)
	exportOnMain := gitOut(t, repo, "ls-tree", "-r", "--name-only", "main")
	require.NotContains(t, exportOnMain, "plans/pending/e2e-sync.yaml")
	exportOnSync := gitOut(t, repo, "ls-tree", "-r", "--name-only", "origin/"+res.Branch)
	require.Contains(t, exportOnSync, "plans/pending/e2e-sync.yaml")

	// Repeat sync is a pure no-op (no new PR activity needed).
	res2, err := syncer.Sync(context.Background(), SyncOptions{
		PlanID: plan.ID, RepoPath: repo, TargetRef: "main",
	})
	require.NoError(t, err)
	require.Equal(t, OutcomeSkipped, res2.Outcome)
	require.True(t, res2.Reused)
	require.Equal(t, res.PRURL, res2.PRURL)
}

func writeFakeGH(t *testing.T, root string) string {
	t.Helper()
	binDir := filepath.Join(root, "bin")
	require.NoError(t, os.MkdirAll(binDir, 0o755))
	script := filepath.Join(binDir, "gh")
	body := `#!/bin/sh
if [ "$1" = "auth" ] && [ "$2" = "status" ]; then
  echo "Logged in to github.com"
  exit 0
fi
if [ "$1" = "pr" ] && [ "$2" = "create" ]; then
  echo "https://example.test/pull/1"
  exit 0
fi
echo "unexpected gh args: $*" >&2
exit 1
`
	require.NoError(t, os.WriteFile(script, []byte(body), 0o755))
	return binDir
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
	return string(out)
}
