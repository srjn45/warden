package daemon

import (
	"context"
	"testing"

	"github.com/srjn45/warden/internal/daemon/oapi"
	"github.com/srjn45/warden/internal/groupstore"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// newGroupServer wires a Server with a real (temp-dir) group store plus the fake
// store/lifecycle the other strict tests use, so join/leave exercise the real
// seating + one-per-project logic.
func newGroupServer(t *testing.T) (*Server, *fakeStore, *fakeLife) {
	t.Helper()
	gs, err := groupstore.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("groupstore.NewStore: %v", err)
	}
	t.Cleanup(func() { gs.Close() })
	fs := newFakeStore()
	fl := &fakeLife{}
	srv := &Server{store: fs, life: fl, groups: gs, hub: newHub(), done: make(chan struct{})}
	return srv, fs, fl
}

// seedAgent inserts a session with the given id and workdir (which drives its
// project key — a non-repo temp dir resolves to a stable `local:` key).
func seedAgent(t *testing.T, fs *fakeStore, id, workdir string) {
	t.Helper()
	if err := fs.Insert(context.Background(), &store.Session{ID: id, Workdir: workdir, Status: store.StatusWorking}); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

func joinReq(name, agentID string) oapi.JoinGroupRequestObject {
	return oapi.JoinGroupRequestObject{Name: name, Body: &oapi.JoinGroupJSONRequestBody{AgentId: agentID}}
}

func leaveReq(name, agentID string) oapi.LeaveGroupRequestObject {
	return oapi.LeaveGroupRequestObject{Name: name, Body: &oapi.LeaveGroupJSONRequestBody{AgentId: agentID}}
}

func TestJoinGroupCreatesSeatsAndFlipsRole(t *testing.T) {
	srv, fs, fl := newGroupServer(t)
	seedAgent(t, fs, "a1", t.TempDir())

	resp, err := srv.JoinGroup(context.Background(), joinReq("backend", "a1"))
	require.NoError(t, err)
	ok := resp.(oapi.JoinGroup200JSONResponse)
	require.Equal(t, "orchestrator", ok.Role)
	require.Equal(t, "backend", ok.Group.Name)
	require.Len(t, ok.Group.Members, 1)
	require.Equal(t, "a1", ok.Group.Members[0].AgentId)
	require.NotEmpty(t, ok.Group.Members[0].ProjectKey)

	// The agent was flipped to the orchestrator role (persisted + relaunched).
	require.Equal(t, "orchestrator", fs.data["a1"].Role)
	require.Equal(t, "a1", fl.switchedRole)
	require.Equal(t, "orchestrator", fl.switchedRoleName)
}

func TestJoinGroupDuplicateProjectConflicts(t *testing.T) {
	srv, fs, _ := newGroupServer(t)
	// Two DIFFERENT agents sharing one workdir ⇒ one project key ⇒ one seat only.
	shared := t.TempDir()
	seedAgent(t, fs, "a1", shared)
	seedAgent(t, fs, "a2", shared)

	_, err := srv.JoinGroup(context.Background(), joinReq("backend", "a1"))
	require.NoError(t, err)

	resp, err := srv.JoinGroup(context.Background(), joinReq("backend", "a2"))
	require.NoError(t, err)
	conflict := resp.(oapi.JoinGroup409JSONResponse)
	require.Equal(t, "a1", conflict.Incumbent.AgentId)
	require.NotEmpty(t, conflict.Error)

	// The rejected joiner was NOT flipped to orchestrator.
	require.Empty(t, fs.data["a2"].Role)
	// The roster still holds exactly the incumbent.
	g, err := srv.groups.Get("backend")
	require.NoError(t, err)
	require.Len(t, g.Members, 1)
	require.Equal(t, "a1", g.Members[0].AgentID)
}

func TestJoinGroupDistinctProjectsCoexist(t *testing.T) {
	srv, fs, _ := newGroupServer(t)
	seedAgent(t, fs, "a1", t.TempDir())
	seedAgent(t, fs, "a2", t.TempDir())

	_, err := srv.JoinGroup(context.Background(), joinReq("team", "a1"))
	require.NoError(t, err)
	resp, err := srv.JoinGroup(context.Background(), joinReq("team", "a2"))
	require.NoError(t, err)
	ok := resp.(oapi.JoinGroup200JSONResponse)
	require.Len(t, ok.Group.Members, 2)
}

func TestJoinGroupSameAgentIsIdempotent(t *testing.T) {
	srv, fs, _ := newGroupServer(t)
	seedAgent(t, fs, "a1", t.TempDir())

	_, err := srv.JoinGroup(context.Background(), joinReq("backend", "a1"))
	require.NoError(t, err)
	resp, err := srv.JoinGroup(context.Background(), joinReq("backend", "a1"))
	require.NoError(t, err)
	ok := resp.(oapi.JoinGroup200JSONResponse)
	require.Len(t, ok.Group.Members, 1, "re-join of own seat must not duplicate")
	require.Equal(t, "orchestrator", ok.Role)
}

func TestLeaveGroupRemovesSeat(t *testing.T) {
	srv, fs, _ := newGroupServer(t)
	seedAgent(t, fs, "a1", t.TempDir())
	seedAgent(t, fs, "a2", t.TempDir())
	_, err := srv.JoinGroup(context.Background(), joinReq("team", "a1"))
	require.NoError(t, err)
	_, err = srv.JoinGroup(context.Background(), joinReq("team", "a2"))
	require.NoError(t, err)

	resp, err := srv.LeaveGroup(context.Background(), leaveReq("team", "a1"))
	require.NoError(t, err)
	g := oapi.Group(resp.(oapi.LeaveGroup200JSONResponse))
	require.Len(t, g.Members, 1)
	require.Equal(t, "a2", g.Members[0].AgentId)

	// Group record persists even after the last seat leaves.
	resp2, err := srv.LeaveGroup(context.Background(), leaveReq("team", "a2"))
	require.NoError(t, err)
	g2 := oapi.Group(resp2.(oapi.LeaveGroup200JSONResponse))
	require.Empty(t, g2.Members)
	_, err = srv.groups.Get("team")
	require.NoError(t, err, "durable group survives an empty roster")
}

func TestLeaveGroupUnknownGroupIs404(t *testing.T) {
	srv, fs, _ := newGroupServer(t)
	seedAgent(t, fs, "a1", t.TempDir())
	_, err := srv.LeaveGroup(context.Background(), leaveReq("nope", "a1"))
	var ae apiError
	require.ErrorAs(t, err, &ae)
	require.Equal(t, 404, ae.code)
}

func TestGroupJoinValidation(t *testing.T) {
	srv, fs, _ := newGroupServer(t)
	seedAgent(t, fs, "a1", t.TempDir())

	// Missing agent_id ⇒ 400.
	_, err := srv.JoinGroup(context.Background(), oapi.JoinGroupRequestObject{Name: "g", Body: &oapi.JoinGroupJSONRequestBody{}})
	var ae apiError
	require.ErrorAs(t, err, &ae)
	require.Equal(t, 400, ae.code)

	// Unknown agent ⇒ 404.
	_, err = srv.JoinGroup(context.Background(), joinReq("g", "ghost"))
	require.ErrorAs(t, err, &ae)
	require.Equal(t, 404, ae.code)
}

func TestGroupsUnconfigured(t *testing.T) {
	srv := &Server{store: newFakeStore(), life: &fakeLife{}, hub: newHub(), done: make(chan struct{})}
	_, err := srv.JoinGroup(context.Background(), joinReq("g", "a1"))
	var ae apiError
	require.ErrorAs(t, err, &ae)
	require.Equal(t, 503, ae.code)
}
