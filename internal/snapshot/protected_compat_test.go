package snapshot

// COMPATIBILITY SUITE — snapshot protected-branch rail (task t1-compat-baseline).
//
// Restore must refuse to reconstruct state onto main/master via
// lifecycle.IsProtectedBranch, and must refuse BEFORE touching the tree.
//
// Later tasks may only change an assertion in this file when their task prompt
// says so, and must say why in the commit message.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/lifecycle"
)

func compatRepo(t *testing.T, branch string) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
		return strings.TrimSpace(string(out))
	}
	run("init", "-q", "-b", branch)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644))
	run("add", ".")
	run("commit", "-q", "-m", "seed")
	return dir
}

func TestCompatRestoreRefusesProtectedBranches(t *testing.T) {
	for _, branch := range []string{"main", "master"} {
		t.Run(branch, func(t *testing.T) {
			dir := compatRepo(t, branch)
			st := newTestStore(t)
			require.NoError(t, st.Put(&Snapshot{ID: "snap-compat", Workdir: dir, Branch: branch, HeadSHA: "deadbeef"}, ""))
			m := New(lifecycle.ExecRunner{}, st)

			_, err := m.Restore(context.Background(), "snap-compat", true)
			require.Error(t, err)
			require.Contains(t, err.Error(), "refusing to restore onto protected branch")
			require.Contains(t, err.Error(), `"`+branch+`"`)
		})
	}
}

func TestCompatRestoreAllowsAgentBranch(t *testing.T) {
	dir := compatRepo(t, "agent/feat")
	st := newTestStore(t)
	require.NoError(t, st.Put(&Snapshot{ID: "snap-compat2", Workdir: dir, Branch: "agent/feat", HeadSHA: "deadbeef"}, ""))
	res, err := New(lifecycle.ExecRunner{}, st).Restore(context.Background(), "snap-compat2", false)
	require.NoError(t, err)
	require.Equal(t, "agent/feat", res.Branch)
	require.False(t, res.Applied, "clean snapshot applies no patch")
	require.False(t, res.HeadMatch, "recorded HEAD differs from the real one")
}
