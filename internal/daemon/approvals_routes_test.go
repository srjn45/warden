package daemon

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/srajanpathak/agentctl/internal/approval"
	"github.com/srajanpathak/agentctl/internal/store"
	"github.com/stretchr/testify/require"
)

func approvalsServer(t *testing.T, fs *fakeStore, fl *fakeLife, on bool) *httptest.Server {
	t.Helper()
	srv := &Server{store: fs, life: fl, approvals: on}
	return httptest.NewServer(srv.router())
}

func TestPostApproveHappyPath(t *testing.T) {
	pane := "│ Do you want to proceed?\n│ ❯ 1. Yes\n│   2. No\n"
	fp := approval.Fingerprint([]string{"Yes", "No"})
	fs := newFakeStore()
	fs.data["a1"] = &store.Session{ID: "a1", TmuxSession: "a1", Status: store.StatusWaitingForInput}
	fl := &fakeLife{output: pane}
	ts := approvalsServer(t, fs, fl, true)
	defer ts.Close()

	body, _ := json.Marshal(ApproveRequest{Option: 1, Fingerprint: fp})
	resp, err := http.Post(ts.URL+"/sessions/a1/approve", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "1", fl.lastKey)
}

func TestPostApproveStaleFingerprint(t *testing.T) {
	fs := newFakeStore()
	fs.data["a1"] = &store.Session{ID: "a1", TmuxSession: "a1", Status: store.StatusWaitingForInput}
	fl := &fakeLife{output: "│ Do you want to proceed?\n│ ❯ 1. Yes\n│   2. No\n"}
	ts := approvalsServer(t, fs, fl, true)
	defer ts.Close()

	body, _ := json.Marshal(ApproveRequest{Option: 1, Fingerprint: "deadbeef"})
	resp, err := http.Post(ts.URL+"/sessions/a1/approve", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	require.Equal(t, http.StatusConflict, resp.StatusCode)
	require.Empty(t, fl.lastKey)
}

func TestPostApproveDisabled(t *testing.T) {
	fs := newFakeStore()
	fs.data["a1"] = &store.Session{ID: "a1", TmuxSession: "a1"}
	ts := approvalsServer(t, fs, &fakeLife{}, false)
	defer ts.Close()
	body, _ := json.Marshal(ApproveRequest{Option: 1, Fingerprint: "x"})
	resp, err := http.Post(ts.URL+"/sessions/a1/approve", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestPostApproveOutOfRange(t *testing.T) {
	pane := "│ Do you want to proceed?\n│ ❯ 1. Yes\n│   2. No\n"
	fp := approval.Fingerprint([]string{"Yes", "No"})
	fs := newFakeStore()
	fs.data["a1"] = &store.Session{ID: "a1", TmuxSession: "a1", Status: store.StatusWaitingForInput}
	ts := approvalsServer(t, fs, &fakeLife{output: pane}, true)
	defer ts.Close()
	body, _ := json.Marshal(ApproveRequest{Option: 9, Fingerprint: fp})
	resp, err := http.Post(ts.URL+"/sessions/a1/approve", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
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

func TestPostApproveUnrecognizedPrompt(t *testing.T) {
	fs := newFakeStore()
	fs.data["a1"] = &store.Session{ID: "a1", TmuxSession: "a1", Status: store.StatusWaitingForInput}
	// pane has no recognizable numbered prompt → Parse returns ok=false
	fl := &fakeLife{output: "Just some working output, no prompt here.\n"}
	ts := approvalsServer(t, fs, fl, true)
	defer ts.Close()
	body, _ := json.Marshal(ApproveRequest{Option: 1, Fingerprint: "anything"})
	resp, err := http.Post(ts.URL+"/sessions/a1/approve", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	require.Equal(t, http.StatusConflict, resp.StatusCode)
	require.Empty(t, fl.lastKey) // never injected
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
