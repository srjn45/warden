package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/snapshot"
	"github.com/srjn45/warden/internal/store"
)

// snapServer builds a server with the snapshot feature wired over a FakeRunner +
// temp store. enabled toggles the config gate.
func snapServer(t *testing.T, fs *fakeStore, fr *lifecycle.FakeRunner, enabled bool) *httptest.Server {
	t.Helper()
	st, err := snapshot.NewStore(filepath.Join(t.TempDir(), "snapshots"))
	require.NoError(t, err)
	srv := &Server{store: fs, snapshots: enabled, snap: snapshot.New(fr, st)}
	return httptest.NewServer(srv.router())
}

func TestSnapshotCreatePinsToSessionWorktreeAndTmux(t *testing.T) {
	fs := newFakeStore()
	_ = fs.Insert(context.Background(), &store.Session{
		ID: "A-1", Workdir: "/repo/.worktrees/A-1", TmuxSession: "A-1", Status: store.StatusWorking,
	})
	fr := &lifecycle.FakeRunner{Responses: map[string]lifecycle.FakeResp{
		"git rev-parse --abbrev-ref HEAD":  {Out: "A-1\n"},
		"git rev-parse HEAD":               {Out: "headsha\n"},
		"git stash create warden snapshot": {Out: "stashsha\n"},
		"git status --porcelain":           {Out: " M foo.go\n"},
		"tmux capture-pane -p -S - -t A-1": {Out: "pane text\n"},
	}}
	ts := snapServer(t, fs, fr, true)
	defer ts.Close()

	// A spoofed dir must be ignored in favour of the agent's own worktree.
	body, _ := json.Marshal(GitRequest{Session: "A-1", Dir: "/repo/.worktrees/OTHER", Message: "checkpoint"})
	resp, err := http.Post(ts.URL+"/api/v1/snapshots", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var snap snapshot.Snapshot
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&snap))
	require.Equal(t, "A-1", snap.SessionID)
	require.Equal(t, "/repo/.worktrees/A-1", snap.Workdir, "pinned to the agent's own worktree, not the spoofed dir")
	require.Equal(t, "stashsha", snap.StashSHA)
	require.NotZero(t, snap.TranscriptLines, "transcript captured from the session's tmux pane")

	// Bookkeeping: a snapshot event is linked to the agent.
	sess, _ := fs.Get(context.Background(), "A-1")
	require.NotEmpty(t, sess.Events)
	require.Equal(t, "snapshot", sess.Events[len(sess.Events)-1].Type)
}

func TestSnapshotDisabledReturns403(t *testing.T) {
	ts := snapServer(t, newFakeStore(), &lifecycle.FakeRunner{}, false)
	defer ts.Close()
	body, _ := json.Marshal(GitRequest{Dir: "/wt"})
	resp, err := http.Post(ts.URL+"/api/v1/snapshots", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestSnapshotRestoreMissingIs404(t *testing.T) {
	ts := snapServer(t, newFakeStore(), &lifecycle.FakeRunner{}, true)
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/api/v1/snapshots/snap-nope/restore", "application/json", bytes.NewReader([]byte(`{}`)))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestSnapshotListFiltersBySession(t *testing.T) {
	fs := newFakeStore()
	fr := &lifecycle.FakeRunner{Responses: map[string]lifecycle.FakeResp{
		"git rev-parse --abbrev-ref HEAD":  {Out: "A-1\n"},
		"git rev-parse HEAD":               {Out: "h\n"},
		"git stash create warden snapshot": {Out: "s\n"},
	}}
	_ = fs.Insert(context.Background(), &store.Session{ID: "A-1", Workdir: "/wt", Status: store.StatusWorking})
	ts := snapServer(t, fs, fr, true)
	defer ts.Close()

	body, _ := json.Marshal(GitRequest{Session: "A-1"})
	resp, err := http.Post(ts.URL+"/api/v1/snapshots", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	listResp, err := http.Get(ts.URL + "/api/v1/snapshots?session=A-1")
	require.NoError(t, err)
	defer listResp.Body.Close()
	var got snapshotListResponse
	require.NoError(t, json.NewDecoder(listResp.Body).Decode(&got))
	require.Len(t, got.Snapshots, 1)
	require.Equal(t, "A-1", got.Snapshots[0].SessionID)

	// A different session has none.
	otherResp, err := http.Get(ts.URL + "/api/v1/snapshots?session=B-2")
	require.NoError(t, err)
	defer otherResp.Body.Close()
	var other snapshotListResponse
	require.NoError(t, json.NewDecoder(otherResp.Body).Decode(&other))
	require.Empty(t, other.Snapshots)
}
