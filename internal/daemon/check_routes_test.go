package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

func TestCheckPinsToSessionWorkdir(t *testing.T) {
	fs := newFakeStore()
	_ = fs.Insert(context.Background(), &store.Session{ID: "A-1", Workdir: "/repo/.worktrees/A-1", Status: store.StatusWorking})
	fl := &fakeLife{checkResult: lifecycle.CheckResult{Passed: true, Checks: []lifecycle.CheckOutcome{{Name: "test", Passed: true}}}}
	ts := lifeServer(t, fs, fl)
	defer ts.Close()

	// A spoofed dir must be ignored in favour of the agent's own worktree.
	body, _ := json.Marshal(CheckRequest{Session: "A-1", Dir: "/repo/.worktrees/OTHER", Name: "test"})
	resp, err := http.Post(ts.URL+"/api/v1/check", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "/repo/.worktrees/A-1", fl.checkDir, "check pinned to the session's own worktree")
	require.Equal(t, "test", fl.checkName)

	var got lifecycle.CheckResult
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	require.True(t, got.Passed)

	// Bookkeeping: a check event is linked to the agent.
	sess, _ := fs.Get(context.Background(), "A-1")
	require.NotEmpty(t, sess.Events)
	require.Equal(t, "check", sess.Events[len(sess.Events)-1].Type)
}

func TestCheckNoConfigIsUnprocessable(t *testing.T) {
	fl := &fakeLife{checkErr: lifecycle.ErrNoCheckConfig}
	ts := lifeServer(t, newFakeStore(), fl)
	defer ts.Close()
	body, _ := json.Marshal(CheckRequest{Dir: "/wt"})
	resp, err := http.Post(ts.URL+"/api/v1/check", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
}

func TestCheckNoDirNoSessionRejected(t *testing.T) {
	ts := lifeServer(t, newFakeStore(), &fakeLife{})
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/api/v1/check", "application/json", bytes.NewReader([]byte(`{}`)))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestCheckDirOnlyNoSession(t *testing.T) {
	fl := &fakeLife{checkResult: lifecycle.CheckResult{Passed: true}}
	ts := lifeServer(t, newFakeStore(), fl)
	defer ts.Close()
	body, _ := json.Marshal(CheckRequest{Dir: "/some/wt"})
	resp, err := http.Post(ts.URL+"/api/v1/check", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "/some/wt", fl.checkDir, "a human run with no session uses the provided dir")
}
