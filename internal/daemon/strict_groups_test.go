package daemon

import (
	"context"
	"strings"
	"testing"

	"github.com/srjn45/warden/internal/daemon/oapi"
	"github.com/srjn45/warden/internal/groupstore"
	"github.com/srjn45/warden/internal/mailbox"
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

// newGroupServerMbox is newGroupServer plus a real (temp-dir) directed-message
// store, so join can exercise the warden-brokered introductions (Stage B4).
func newGroupServerMbox(t *testing.T) (*Server, *fakeStore, *fakeLife, *mailbox.Store) {
	t.Helper()
	srv, fs, fl := newGroupServer(t)
	mb, err := mailbox.New(t.TempDir())
	if err != nil {
		t.Fatalf("mailbox.New: %v", err)
	}
	t.Cleanup(func() { mb.Close() })
	srv.mbox = mb
	return srv, fs, fl, mb
}

// seedAgent inserts a session with the given id and workdir (which drives its
// project key — a non-repo temp dir resolves to a stable `local:` key).
func seedAgent(t *testing.T, fs *fakeStore, id, workdir string) {
	t.Helper()
	if err := fs.Insert(context.Background(), &store.Session{ID: id, Workdir: workdir, Status: store.StatusWorking}); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

// seedNamedAgent inserts a session with an explicit alias and status so intro
// content (name in the descriptor) and wake behaviour (parked recipients) are
// testable.
func seedNamedAgent(t *testing.T, fs *fakeStore, id, name, workdir string, st store.Status) {
	t.Helper()
	if err := fs.Insert(context.Background(), &store.Session{ID: id, Name: name, Workdir: workdir, Status: st}); err != nil {
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

// introBodies reads a recipient's inbox and returns just the message bodies.
func introBodies(t *testing.T, mb *mailbox.Store, to string) []string {
	t.Helper()
	msgs, err := mb.Messages(to)
	require.NoError(t, err)
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		require.Equal(t, "daemon", m.From, "intros are stamped with the reserved daemon provenance")
		out = append(out, m.Body)
	}
	return out
}

// TestJoinGroupBrokersIntroductionsBothDirections is the B4 acceptance: on join,
// each of the N existing members receives exactly one intro announcing the
// joiner, and the joiner receives exactly N intros (one per existing member),
// with zero agent turns.
func TestJoinGroupBrokersIntroductionsBothDirections(t *testing.T) {
	srv, fs, _, mb := newGroupServerMbox(t)
	seedNamedAgent(t, fs, "a1", "alpha", t.TempDir(), store.StatusWorking)
	seedNamedAgent(t, fs, "a2", "beta", t.TempDir(), store.StatusWorking)
	seedNamedAgent(t, fs, "a3", "gamma", t.TempDir(), store.StatusWorking)

	// First seat: no existing members ⇒ nobody is introduced.
	_, err := srv.JoinGroup(context.Background(), joinReq("team", "a1"))
	require.NoError(t, err)
	require.Empty(t, introBodies(t, mb, "a1"), "first joiner has no peers to be introduced to")

	// Second seat: a1 (existing) learns about a2; a2 (joiner) learns about a1.
	_, err = srv.JoinGroup(context.Background(), joinReq("team", "a2"))
	require.NoError(t, err)

	a1 := introBodies(t, mb, "a1")
	require.Len(t, a1, 1)
	require.Contains(t, a1[0], "a2")
	require.Contains(t, a1[0], "beta", "descriptor carries the joiner's alias")
	require.Contains(t, a1[0], "joined")

	a2 := introBodies(t, mb, "a2")
	require.Len(t, a2, 1)
	require.Contains(t, a2[0], "a1")
	require.Contains(t, a2[0], "alpha")

	// Third seat (N=2 existing): a1 and a2 each get one more; a3 gets two.
	_, err = srv.JoinGroup(context.Background(), joinReq("team", "a3"))
	require.NoError(t, err)

	require.Len(t, introBodies(t, mb, "a1"), 2, "each existing member gets exactly one intro per join")
	require.Len(t, introBodies(t, mb, "a2"), 2, "a2: one from its own join (a1) + one when a3 joined")

	a3 := introBodies(t, mb, "a3")
	require.Len(t, a3, 2, "joiner receives one intro per existing member (N=2)")
	joined := strings.Join(a3, "\n")
	require.Contains(t, joined, "a1")
	require.Contains(t, joined, "a2")
}

// TestJoinGroupIdempotentRejoinDoesNotReBroker guards against re-announcing on an
// idempotent re-join of the agent's own seat.
func TestJoinGroupIdempotentRejoinDoesNotReBroker(t *testing.T) {
	srv, fs, _, mb := newGroupServerMbox(t)
	seedNamedAgent(t, fs, "a1", "alpha", t.TempDir(), store.StatusWorking)
	seedNamedAgent(t, fs, "a2", "beta", t.TempDir(), store.StatusWorking)

	_, err := srv.JoinGroup(context.Background(), joinReq("team", "a1"))
	require.NoError(t, err)
	_, err = srv.JoinGroup(context.Background(), joinReq("team", "a2"))
	require.NoError(t, err)
	require.Len(t, introBodies(t, mb, "a1"), 1)

	// a2 re-joins its own seat: idempotent ⇒ no new intros in either inbox.
	_, err = srv.JoinGroup(context.Background(), joinReq("team", "a2"))
	require.NoError(t, err)
	require.Len(t, introBodies(t, mb, "a1"), 1, "idempotent re-join must not re-announce")
	require.Len(t, introBodies(t, mb, "a2"), 1)
}

// TestJoinGroupIntroWakesParkedRecipient checks a parked (idle) existing member is
// nudged to read, while the join still succeeds.
func TestJoinGroupIntroWakesParkedRecipient(t *testing.T) {
	srv, fs, fl, _ := newGroupServerMbox(t)
	seedNamedAgent(t, fs, "a1", "alpha", t.TempDir(), store.StatusIdle)
	seedNamedAgent(t, fs, "a2", "beta", t.TempDir(), store.StatusWorking)

	_, err := srv.JoinGroup(context.Background(), joinReq("team", "a1"))
	require.NoError(t, err)
	// Joining flips a1 to the orchestrator role, which momentarily marks it
	// spawning; simulate it settling back to idle before its peer arrives.
	require.NoError(t, fs.UpdateStatus(context.Background(), "a1", store.StatusIdle))

	_, err = srv.JoinGroup(context.Background(), joinReq("team", "a2"))
	require.NoError(t, err)

	require.Equal(t, groupIntroNotice, fl.lastInput, "a parked recipient is nudged to read its inbox")
}

// TestJoinGroupIntrosNoMboxIsNoop confirms join still works when messaging is
// unconfigured (nil mbox) — intros are simply skipped.
func TestJoinGroupIntrosNoMboxIsNoop(t *testing.T) {
	srv, fs, _ := newGroupServer(t) // no mbox wired
	seedAgent(t, fs, "a1", t.TempDir())
	seedAgent(t, fs, "a2", t.TempDir())

	_, err := srv.JoinGroup(context.Background(), joinReq("team", "a1"))
	require.NoError(t, err)
	resp, err := srv.JoinGroup(context.Background(), joinReq("team", "a2"))
	require.NoError(t, err)
	require.Len(t, resp.(oapi.JoinGroup200JSONResponse).Group.Members, 2)
}
