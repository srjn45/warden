package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/srajanpathak/agentctl/internal/store"
	"github.com/stretchr/testify/require"
)

func approvalsServer(t *testing.T, fs *fakeStore, fl *fakeLife, on bool) *httptest.Server {
	t.Helper()
	srv := &Server{store: fs, life: fl, approvals: on}
	return httptest.NewServer(srv.router())
}

func TestGetApprovalsDisabled(t *testing.T) {
	ts := approvalsServer(t, newFakeStore(), &fakeLife{}, false)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/approvals")
	require.NoError(t, err)
	var out approvalsResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	require.False(t, out.Enabled)
	require.Empty(t, out.Approvals)
}

func TestGetApprovalsListsWaiting(t *testing.T) {
	fs := newFakeStore()
	fs.data["a1"] = &store.Session{
		ID: "a1", TmuxSession: "a1", Status: store.StatusWaitingForInput,
		LastPaneExcerpt: "│ Do you want to proceed?\n│ ❯ 1. Yes\n│   2. No\n",
	}
	fs.data["a2"] = &store.Session{ID: "a2", Status: store.StatusWorking}
	ts := approvalsServer(t, fs, &fakeLife{}, true)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/approvals")
	require.NoError(t, err)
	var out approvalsResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	require.True(t, out.Enabled)
	require.Len(t, out.Approvals, 1) // only the waiting agent
	require.Equal(t, "a1", out.Approvals[0].ID)
	require.True(t, out.Approvals[0].Recognized)
}
