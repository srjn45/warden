package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/snapshot"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

func TestGitCommitPinsToSessionWorkdir(t *testing.T) {
	fs := newFakeStore()
	_ = fs.Insert(context.Background(), &store.Session{ID: "A-1", Workdir: "/repo/.worktrees/A-1", Status: store.StatusWorking})
	fl := &fakeLife{gitCommitResult: lifecycle.CommitResult{Committed: true, SHA: "abc1234", Branch: "A-1"}}
	ts := lifeServer(t, fs, fl)
	defer ts.Close()

	// Empty dir pins to the agent's own worktree.
	body, _ := json.Marshal(GitRequest{Session: "A-1", Message: "do x"})
	resp, err := http.Post(ts.URL+"/api/v1/git/commit", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "/repo/.worktrees/A-1", fl.gitCommitDir, "commit pinned to the session's own worktree")
	require.Equal(t, "do x", fl.gitCommitMsg)

	var got lifecycle.CommitResult
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	require.True(t, got.Committed)
	require.Equal(t, "abc1234", got.SHA)

	// Bookkeeping: a commit event is linked to the agent.
	sess, _ := fs.Get(context.Background(), "A-1")
	require.NotEmpty(t, sess.Events)
	require.Equal(t, "commit", sess.Events[len(sess.Events)-1].Type)
}

func TestPinnedWorkdirHonorsLinkedWorktreeAcrossOps(t *testing.T) {
	repo, wt := setupRepoWithWorktree(t)
	fs := newFakeStore()
	_ = fs.Insert(context.Background(), &store.Session{
		ID: "A-1", Repo: repo, Workdir: repo, TmuxSession: "A-1", Status: store.StatusWorking,
	})
	fl := &fakeLife{
		gitCommitResult: lifecycle.CommitResult{Committed: true, SHA: "deadbeef", Branch: "wt-branch"},
		gitPushResult:   lifecycle.PushResult{Branch: "wt-branch", Remote: "origin", Pushed: true},
		gitSyncResult:   lifecycle.SyncResult{Branch: "wt-branch", Base: "main", Updated: true},
		checkResult:     lifecycle.CheckResult{Passed: true},
	}
	ts := lifeServer(t, fs, fl)
	defer ts.Close()

	// commit
	closeOK(t, postGitOK(t, ts.URL+"/api/v1/git/commit", GitRequest{Session: "A-1", Dir: wt, Message: "m"}))
	require.Equal(t, wt, fl.gitCommitDir)

	// push
	closeOK(t, postGitOK(t, ts.URL+"/api/v1/git/push", GitRequest{Session: "A-1", Dir: wt}))
	require.Equal(t, wt, fl.gitPushDir)

	// sync
	closeOK(t, postGitOK(t, ts.URL+"/api/v1/git/sync", GitRequest{Session: "A-1", Dir: wt, Base: "main"}))
	require.Equal(t, wt, fl.gitSyncDir)

	// check
	closeOK(t, postGitOK(t, ts.URL+"/api/v1/check", CheckRequest{Session: "A-1", Dir: wt}))
	require.Equal(t, wt, fl.checkDir)

	// snapshot
	fr := &lifecycle.FakeRunner{Responses: map[string]lifecycle.FakeResp{
		"git rev-parse --abbrev-ref HEAD":  {Out: "wt-branch\n"},
		"git rev-parse HEAD":               {Out: "headsha\n"},
		"git stash create warden snapshot": {Out: "stashsha\n"},
		"git status --porcelain":           {Out: ""},
		"tmux capture-pane -p -S - -t A-1": {Out: "pane\n"},
	}}
	snapTS := snapServer(t, fs, fr, true)
	defer snapTS.Close()
	resp := postGitOK(t, snapTS.URL+"/api/v1/snapshots", GitRequest{Session: "A-1", Dir: wt, Message: "checkpoint"})
	var snap snapshot.Snapshot
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&snap))
	resp.Body.Close()
	require.Equal(t, wt, snap.Workdir, "snapshot must honor linked worktree dir")
}

func TestPinnedWorkdirRejectsInvalidDirs(t *testing.T) {
	repo, _ := setupRepoWithWorktree(t)
	fs := newFakeStore()
	_ = fs.Insert(context.Background(), &store.Session{
		ID: "A-1", Repo: repo, Workdir: repo, Status: store.StatusWorking,
	})
	fl := &fakeLife{gitCommitResult: lifecycle.CommitResult{Committed: true, SHA: "should-not-run"}}
	ts := lifeServer(t, fs, fl)
	defer ts.Close()

	missing := filepath.Join(t.TempDir(), "does-not-exist")
	filePath := filepath.Join(t.TempDir(), "not-a-dir")
	require.NoError(t, os.WriteFile(filePath, []byte("x"), 0o644))
	nonGit := t.TempDir()

	unrelated := t.TempDir()
	out, err := exec.Command("git", "-C", unrelated, "init").CombinedOutput()
	require.NoError(t, err, "git init unrelated: %s", out)

	bare := filepath.Join(t.TempDir(), "bare.git")
	out, err = exec.Command("git", "init", "--bare", bare).CombinedOutput()
	require.NoError(t, err, "git init --bare: %s", out)

	dotGit := filepath.Join(repo, ".git")
	require.DirExists(t, dotGit)

	cases := []struct {
		name   string
		dir    string
		status int
		substr string
	}{
		{"missing", missing, http.StatusBadRequest, "not a usable directory"},
		{"file", filePath, http.StatusBadRequest, "not a directory"},
		{"non-git", nonGit, http.StatusBadRequest, "not a git worktree"},
		{"unrelated-repo", unrelated, http.StatusForbidden, "outside the agent's repository"},
		{"bare-repo", bare, http.StatusBadRequest, "bare git repository"},
		{"dot-git-dir", dotGit, http.StatusBadRequest, "git dir (.git)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fl.gitCommitDir = ""
			body, _ := json.Marshal(GitRequest{Session: "A-1", Dir: tc.dir, Message: "spoof"})
			resp, err := http.Post(ts.URL+"/api/v1/git/commit", "application/json", bytes.NewReader(body))
			require.NoError(t, err)
			defer resp.Body.Close()
			require.Equal(t, tc.status, resp.StatusCode)
			require.Empty(t, fl.gitCommitDir, "must not fall back to sess.Workdir")
			raw, _ := io.ReadAll(resp.Body)
			require.Contains(t, string(raw), tc.substr)
		})
	}
}

func TestPinnedWorkdirSymlinkAliasSameRepoHonored(t *testing.T) {
	repo, wt := setupRepoWithWorktree(t)
	alias := filepath.Join(t.TempDir(), "wt-alias")
	require.NoError(t, os.Symlink(wt, alias))

	fs := newFakeStore()
	_ = fs.Insert(context.Background(), &store.Session{
		ID: "A-1", Repo: repo, Workdir: repo, Status: store.StatusWorking,
	})
	fl := &fakeLife{gitCommitResult: lifecycle.CommitResult{Committed: true, SHA: "symlink"}}
	ts := lifeServer(t, fs, fl)
	defer ts.Close()

	closeOK(t, postGitOK(t, ts.URL+"/api/v1/git/commit", GitRequest{Session: "A-1", Dir: alias, Message: "via alias"}))
	require.Equal(t, alias, fl.gitCommitDir, "same-repo symlink alias must be honored as the explicit dir")
}

func TestPinnedWorkdirSymlinkToUnrelatedRepoRejected(t *testing.T) {
	repo, _ := setupRepoWithWorktree(t)
	other := t.TempDir()
	out, err := exec.Command("git", "-C", other, "init").CombinedOutput()
	require.NoError(t, err, "git init other: %s", out)
	alias := filepath.Join(t.TempDir(), "evil-alias")
	require.NoError(t, os.Symlink(other, alias))

	fs := newFakeStore()
	_ = fs.Insert(context.Background(), &store.Session{
		ID: "A-1", Repo: repo, Workdir: repo, Status: store.StatusWorking,
	})
	fl := &fakeLife{gitCommitResult: lifecycle.CommitResult{Committed: true, SHA: "should-not-run"}}
	ts := lifeServer(t, fs, fl)
	defer ts.Close()

	body, _ := json.Marshal(GitRequest{Session: "A-1", Dir: alias, Message: "spoof"})
	resp, err := http.Post(ts.URL+"/api/v1/git/commit", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	require.Empty(t, fl.gitCommitDir)
	raw, _ := io.ReadAll(resp.Body)
	require.Contains(t, string(raw), "outside the agent's repository")
}

func TestPinnedWorkdirSymlinkToSessionWorkdirPins(t *testing.T) {
	repo, _ := setupRepoWithWorktree(t)
	alias := filepath.Join(t.TempDir(), "repo-alias")
	require.NoError(t, os.Symlink(repo, alias))

	fs := newFakeStore()
	_ = fs.Insert(context.Background(), &store.Session{
		ID: "A-1", Repo: repo, Workdir: repo, Status: store.StatusWorking,
	})
	fl := &fakeLife{gitCommitResult: lifecycle.CommitResult{Committed: true, SHA: "pin"}}
	ts := lifeServer(t, fs, fl)
	defer ts.Close()

	// Symlink that resolves to sess.Workdir is treated as a match → pin to Workdir.
	closeOK(t, postGitOK(t, ts.URL+"/api/v1/git/commit", GitRequest{Session: "A-1", Dir: alias, Message: "via workdir alias"}))
	require.Equal(t, repo, fl.gitCommitDir)
}

func TestGitCommitRailErrorIsConflict(t *testing.T) {
	fl := &fakeLife{gitCommitErr: errors.New("refusing to commit on protected branch \"main\"")}
	ts := lifeServer(t, newFakeStore(), fl)
	defer ts.Close()
	body, _ := json.Marshal(GitRequest{Dir: "/wt", Message: "m"})
	resp, err := http.Post(ts.URL+"/api/v1/git/commit", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusConflict, resp.StatusCode)
}

func TestGitCommitDirOnlyNoSession(t *testing.T) {
	fl := &fakeLife{gitCommitResult: lifecycle.CommitResult{Committed: true, SHA: "z", Branch: "b"}}
	ts := lifeServer(t, newFakeStore(), fl)
	defer ts.Close()
	body, _ := json.Marshal(GitRequest{Dir: "/some/wt", Message: "m"})
	resp, err := http.Post(ts.URL+"/api/v1/git/commit", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "/some/wt", fl.gitCommitDir, "a human run with no session uses the provided dir")
}

func TestGitCommitNoDirNoSessionRejected(t *testing.T) {
	ts := lifeServer(t, newFakeStore(), &fakeLife{})
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/api/v1/git/commit", "application/json", strings.NewReader(`{"message":"m"}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestGitCommitUnknownSessionFallsBackToDir(t *testing.T) {
	fl := &fakeLife{gitCommitResult: lifecycle.CommitResult{Committed: true}}
	ts := lifeServer(t, newFakeStore(), fl)
	defer ts.Close()
	body, _ := json.Marshal(GitRequest{Session: "ghost", Dir: "/fallback", Message: "m"})
	resp, err := http.Post(ts.URL+"/api/v1/git/commit", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "/fallback", fl.gitCommitDir)
}

func TestGitPush(t *testing.T) {
	fs := newFakeStore()
	_ = fs.Insert(context.Background(), &store.Session{ID: "A-1", Workdir: "/repo/.worktrees/A-1", Status: store.StatusWorking})
	fl := &fakeLife{gitPushResult: lifecycle.PushResult{Branch: "A-1", Remote: "origin", Pushed: true}}
	ts := lifeServer(t, fs, fl)
	defer ts.Close()
	body, _ := json.Marshal(GitRequest{Session: "A-1"})
	resp, err := http.Post(ts.URL+"/api/v1/git/push", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "/repo/.worktrees/A-1", fl.gitPushDir)

	var got lifecycle.PushResult
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	require.True(t, got.Pushed)
}

func TestGitSyncReturnsConflicts(t *testing.T) {
	fl := &fakeLife{gitSyncResult: lifecycle.SyncResult{Branch: "f", Base: "main", Conflicts: []string{"a.go"}}}
	ts := lifeServer(t, newFakeStore(), fl)
	defer ts.Close()
	body, _ := json.Marshal(GitRequest{Dir: "/wt", Base: "main"})
	resp, err := http.Post(ts.URL+"/api/v1/git/sync", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "main", fl.gitSyncBase)

	var got lifecycle.SyncResult
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	require.Equal(t, []string{"a.go"}, got.Conflicts)
	require.False(t, got.Updated)
}

func postGitOK(t *testing.T, url string, body any) *http.Response {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	resp, err := http.Post(url, "application/json", bytes.NewReader(raw))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	return resp
}

func closeOK(t *testing.T, resp *http.Response) {
	t.Helper()
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

// setupRepoWithWorktree creates a temp git repo and a linked worktree; returns
// (repoRoot, worktreePath).
func setupRepoWithWorktree(t *testing.T) (repo, wt string) {
	t.Helper()
	repo = t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "%v: %s", args, out)
	}
	run("git", "init", "-b", "main")
	run("git", "config", "user.email", "test@example.com")
	run("git", "config", "user.name", "test")
	run("git", "commit", "--allow-empty", "-m", "init")
	wt = filepath.Join(t.TempDir(), "wt")
	run("git", "worktree", "add", "-b", "wt-branch", wt)
	require.DirExists(t, wt)
	var err error
	repo, err = filepath.Abs(repo)
	require.NoError(t, err)
	wt, err = filepath.Abs(wt)
	require.NoError(t, err)
	return repo, wt
}
