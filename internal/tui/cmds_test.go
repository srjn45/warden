package tui

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSpawnCmdUsesGivenCwd(t *testing.T) {
	f := &fakeAPI{}
	msg := spawnCmd(f, "do the thing", "my-agent", "/work/api", "reviewer", "aider", false)()
	done, ok := msg.(spawnDoneMsg)
	require.True(t, ok)
	require.NoError(t, done.err)
	require.NotNil(t, f.spawned)
	require.Equal(t, "/work/api", f.spawned.Cwd)
	require.Equal(t, "do the thing", f.spawned.Prompt)
	require.Equal(t, "my-agent", f.spawned.Name)
	require.Equal(t, "reviewer", f.spawned.Role)
	require.Equal(t, "aider", f.spawned.Backend)
}

// TestNewProjectCmdScaffoldsAndCommits proves newProjectCmd creates the dir,
// writes a README titled after the project, and leaves a git repo with one
// commit — the "New" option's backing operation for modeOpenProjectNew.
func TestNewProjectCmdScaffoldsAndCommits(t *testing.T) {
	requireGit(t)
	dir := filepath.Join(t.TempDir(), "my-project")

	msg := newProjectCmd(dir, "my-project")()
	done, ok := msg.(newProjectMsg)
	require.True(t, ok)
	require.NoError(t, done.err)
	require.Equal(t, dir, done.dir)

	readme, err := os.ReadFile(filepath.Join(dir, "README.md"))
	require.NoError(t, err)
	require.Equal(t, "# my-project\n", string(readme))

	require.DirExists(t, filepath.Join(dir, ".git"))
	out, err := exec.Command("git", "-C", dir, "log", "--oneline").Output()
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(string(out), "\n"), "exactly one commit")
	require.Contains(t, string(out), "chore: project initiated using warden")
}

// TestNewProjectCmdRefusesExistingDir proves it never clobbers a directory
// the caller didn't just create.
func TestNewProjectCmdRefusesExistingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "already-here")
	require.NoError(t, os.Mkdir(dir, 0o755))

	msg := newProjectCmd(dir, "already-here")()
	done, ok := msg.(newProjectMsg)
	require.True(t, ok)
	require.Error(t, done.err)
	require.Contains(t, done.err.Error(), "already exists")
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	// newProjectCmd shells out to plain `git commit`, which needs an identity;
	// isolate this test from the environment's global/system git config so it
	// doesn't depend on the machine having user.name/user.email set.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig"))
	t.Setenv("GIT_AUTHOR_NAME", "warden-test")
	t.Setenv("GIT_AUTHOR_EMAIL", "warden-test@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "warden-test")
	t.Setenv("GIT_COMMITTER_EMAIL", "warden-test@example.com")
}
