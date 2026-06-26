package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// gitRun runs git in dir and fails the test on error.
func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
}

// initRepoWithWorktree builds a git repo with one commit and a linked worktree,
// returning (mainRepo, linkedWorktree).
func initRepoWithWorktree(t *testing.T) (string, string) {
	t.Helper()
	base := t.TempDir()
	main := filepath.Join(base, "main")
	require.NoError(t, os.MkdirAll(main, 0o755))
	gitRun(t, main, "init", "-q", "-b", "main")
	require.NoError(t, os.WriteFile(filepath.Join(main, "seed.txt"), []byte("x"), 0o644))
	gitRun(t, main, "add", ".")
	gitRun(t, main, "commit", "-q", "-m", "seed")

	linked := filepath.Join(base, "linked")
	gitRun(t, main, "worktree", "add", "-q", "-b", "feature", linked)
	return main, linked
}

func TestDetectRootWrite_MainWorktreeDenied(t *testing.T) {
	main, _ := initRepoWithWorktree(t)
	reason := detectRootWrite(context.Background(), "Edit", filepath.Join(main, "seed.txt"), main)
	require.NotEmpty(t, reason, "an edit to the main worktree must be denied")
	require.Contains(t, reason, "main repo working tree")
}

func TestDetectRootWrite_NewFileInMainWorktreeDenied(t *testing.T) {
	main, _ := initRepoWithWorktree(t)
	// A not-yet-existing file in a not-yet-existing subdir still resolves to the
	// main worktree via the nearest existing ancestor.
	target := filepath.Join(main, "new", "deep", "file.go")
	reason := detectRootWrite(context.Background(), "Write", target, main)
	require.NotEmpty(t, reason)
}

func TestDetectRootWrite_LinkedWorktreeAllowed(t *testing.T) {
	_, linked := initRepoWithWorktree(t)
	reason := detectRootWrite(context.Background(), "Edit", filepath.Join(linked, "seed.txt"), linked)
	require.Empty(t, reason, "an edit inside a linked worktree must be allowed")
}

func TestDetectRootWrite_RelativePathResolvedAgainstCwd(t *testing.T) {
	main, _ := initRepoWithWorktree(t)
	reason := detectRootWrite(context.Background(), "Edit", "seed.txt", main)
	require.NotEmpty(t, reason, "a relative path under the main worktree must be denied")
}

func TestDetectRootWrite_NonGuardedToolAllowed(t *testing.T) {
	main, _ := initRepoWithWorktree(t)
	require.Empty(t, detectRootWrite(context.Background(), "Bash", filepath.Join(main, "seed.txt"), main))
}

func TestDetectRootWrite_OutsideRepoAllowed(t *testing.T) {
	dir := t.TempDir() // not a git repo
	require.Empty(t, detectRootWrite(context.Background(), "Edit", filepath.Join(dir, "f.txt"), dir))
}

func TestDetectRootWrite_EmptyPathAllowed(t *testing.T) {
	require.Empty(t, detectRootWrite(context.Background(), "Edit", "", "/somewhere"))
}

func TestDetectRootWrite_RelativePathNoCwdAllowed(t *testing.T) {
	require.Empty(t, detectRootWrite(context.Background(), "Edit", "rel.txt", ""))
}
