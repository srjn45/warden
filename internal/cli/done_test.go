package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// doneStub records which session endpoints were hit and serves a canned PR
// result so the `done` command's ordering can be asserted.
func doneStub(t *testing.T, prResult any, prStatus int) (addr string, hits *[]string) {
	t.Helper()
	var mu sync.Mutex
	var paths []string
	hits = &paths
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/create-pr"):
			if prStatus != 0 {
				w.WriteHeader(prStatus)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "boom"})
				return
			}
			_ = json.NewEncoder(w).Encode(prResult)
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{})
		}
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://"), hits
}

func TestDoneCreatePROpensThenFinishes(t *testing.T) {
	addr, hits := doneStub(t, map[string]any{"url": "https://github.com/o/r/pull/5", "created": true}, 0)
	out, err := runGit(t, addr, "done", "A-1", "--create-pr")
	require.NoError(t, err)
	require.Contains(t, out, "opened PR: https://github.com/o/r/pull/5")
	require.Contains(t, out, "done A-1")
	// PR creation must run before terminate/delete.
	require.Equal(t, []string{
		"/api/v1/sessions/A-1/create-pr",
		"/api/v1/sessions/A-1/terminate",
		"/api/v1/sessions/A-1/delete",
	}, *hits)
}

func TestDoneCreatePRExistingPRReported(t *testing.T) {
	addr, _ := doneStub(t, map[string]any{"url": "https://github.com/o/r/pull/2", "created": false}, 0)
	out, err := runGit(t, addr, "done", "A-1", "--create-pr")
	require.NoError(t, err)
	require.Contains(t, out, "PR already exists: https://github.com/o/r/pull/2")
}

func TestDoneCreatePRFailureLeavesAgentRunning(t *testing.T) {
	addr, hits := doneStub(t, nil, http.StatusConflict)
	_, err := runGit(t, addr, "done", "A-1", "--create-pr")
	require.Error(t, err, "a PR failure aborts done")
	require.Contains(t, err.Error(), "create PR")
	// Terminate/delete must NOT have run.
	require.Equal(t, []string{"/api/v1/sessions/A-1/create-pr"}, *hits)
}

func TestDoneWithoutCreatePRSkipsPR(t *testing.T) {
	addr, hits := doneStub(t, nil, 0)
	out, err := runGit(t, addr, "done", "A-1")
	require.NoError(t, err)
	require.Contains(t, out, "done A-1")
	require.Equal(t, []string{"/api/v1/sessions/A-1/terminate", "/api/v1/sessions/A-1/delete"}, *hits)
}
