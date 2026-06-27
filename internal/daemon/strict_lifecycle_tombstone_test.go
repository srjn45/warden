package daemon

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// deleteSession drives POST /sessions/{id}/delete and returns the decoded body.
func deleteSession(t *testing.T, ts *httptest.Server, id string, hard bool) (int, struct {
	Status  string `json:"status"`
	Warning string `json:"warning"`
}) {
	t.Helper()
	body, _ := json.Marshal(map[string]bool{"hard": hard})
	resp, err := http.Post(ts.URL+"/api/v1/sessions/"+id+"/delete", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	var out struct {
		Status  string `json:"status"`
		Warning string `json:"warning"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// TestDeleteTombstonesParentWithLiveChild: deleting a parent that still has a
// live child keeps the record active + terminal (not archived) and tears its
// tmux down, so the children stay anchored under it in the sub-tree view.
func TestDeleteTombstonesParentWithLiveChild(t *testing.T) {
	fs := newFakeStore()
	fs.data["parent"] = &store.Session{ID: "parent", TmuxSession: "parent", Status: store.StatusIdle}
	fs.data["child"] = &store.Session{ID: "child", ParentID: "parent", Status: store.StatusWorking}
	fl := &fakeLife{}
	ts := lifeServer(t, fs, fl)
	defer ts.Close()

	code, out := deleteSession(t, ts, "parent", false)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, "tombstoned", out.Status)

	require.Equal(t, "parent", fl.terminated, "tmux must be torn down")
	got, ok := fs.data["parent"]
	require.True(t, ok, "tombstone must stay an active record (not archived)")
	require.Equal(t, store.StatusDone, got.Status, "soft delete → done")
	require.Empty(t, fs.closed["parent"], "tombstone must not be archived")
}

// A hard delete of a parent with a live child still tombstones, but with the
// force-kill terminal status.
func TestHardDeleteTombstonesParentAsOrphaned(t *testing.T) {
	fs := newFakeStore()
	fs.data["parent"] = &store.Session{ID: "parent", TmuxSession: "parent", Status: store.StatusWorking}
	fs.data["child"] = &store.Session{ID: "child", ParentID: "parent", Status: store.StatusWorking}
	fl := &fakeLife{}
	ts := lifeServer(t, fs, fl)
	defer ts.Close()

	code, out := deleteSession(t, ts, "parent", true)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, "tombstoned", out.Status)
	require.Equal(t, store.StatusOrphaned, fs.data["parent"].Status, "hard delete → orphaned")
}

// A childless parent deletes exactly as before: soft → archive, hard → remove.
func TestDeleteChildlessParentUnchanged(t *testing.T) {
	t.Run("soft archives", func(t *testing.T) {
		fs := newFakeStore()
		fs.data["solo"] = &store.Session{ID: "solo", TmuxSession: "solo", Status: store.StatusDone}
		ts := lifeServer(t, fs, &fakeLife{})
		defer ts.Close()

		code, out := deleteSession(t, ts, "solo", false)
		require.Equal(t, http.StatusOK, code)
		require.Equal(t, "deleted", out.Status)
		require.NotContains(t, fs.data, "solo", "soft delete archives out of active")
		require.Contains(t, fs.closed, "solo")
	})
	t.Run("hard removes", func(t *testing.T) {
		fs := newFakeStore()
		fs.data["solo"] = &store.Session{ID: "solo", TmuxSession: "solo", Status: store.StatusDone}
		ts := lifeServer(t, fs, &fakeLife{})
		defer ts.Close()

		code, out := deleteSession(t, ts, "solo", true)
		require.Equal(t, http.StatusOK, code)
		require.Equal(t, "deleted", out.Status)
		require.NotContains(t, fs.data, "solo")
		require.NotContains(t, fs.closed, "solo")
	})
}

// A parent whose only child is already terminal has no LIVE children, so it
// deletes normally (no tombstone).
func TestDeleteParentWithOnlyTerminalChild(t *testing.T) {
	fs := newFakeStore()
	fs.data["parent"] = &store.Session{ID: "parent", TmuxSession: "parent", Status: store.StatusIdle}
	fs.data["child"] = &store.Session{ID: "child", ParentID: "parent", Status: store.StatusDone}
	ts := lifeServer(t, fs, &fakeLife{})
	defer ts.Close()

	code, out := deleteSession(t, ts, "parent", false)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, "deleted", out.Status, "a terminal-only child is not a live child")
	require.NotContains(t, fs.data, "parent")
}
