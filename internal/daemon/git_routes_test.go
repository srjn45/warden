package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

func TestGitCommitPinsToSessionWorkdir(t *testing.T) {
	fs := newFakeStore()
	_ = fs.Insert(context.Background(), &store.Session{ID: "A-1", Workdir: "/repo/.worktrees/A-1", Status: store.StatusWorking})
	fl := &fakeLife{gitCommitResult: lifecycle.CommitResult{Committed: true, SHA: "abc1234", Branch: "A-1"}}
	ts := lifeServer(t, fs, fl)
	defer ts.Close()

	// A spoofed dir must be ignored in favour of the agent's own worktree.
	body, _ := json.Marshal(GitRequest{Session: "A-1", Dir: "/repo/.worktrees/OTHER", Message: "do x"})
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
