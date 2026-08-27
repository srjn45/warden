package daemon

import (
	"context"
	"strings"
	"testing"

	"github.com/srjn45/warden/internal/daemon/oapi"
	"github.com/srjn45/warden/internal/mailbox"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// newGroupServerWithMbox is newGroupServer plus a real (temp-dir) mailbox so the
// terminate teardown's abandonment notices can be asserted.
func newGroupServerWithMbox(t *testing.T) (*Server, *fakeStore, *fakeLife, *mailbox.Store) {
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

// termReq builds a group-aware terminate (a non-nil body opts into the gate).
func termReq(id string, confirm bool) oapi.TerminateSessionRequestObject {
	return oapi.TerminateSessionRequestObject{Id: id, Body: &oapi.TerminateSessionJSONRequestBody{Confirm: confirm}}
}

// legacyTermReq builds a bodyless terminate (the rotate/self-succession path).
func legacyTermReq(id string) oapi.TerminateSessionRequestObject {
	return oapi.TerminateSessionRequestObject{Id: id}
}

// seedTmux seeds an agent whose tmux session id matches its id, so the fake
// lifecycle records the terminate against a non-empty session.
func seedTmux(t *testing.T, fs *fakeStore, id string) {
	t.Helper()
	seedAgent(t, fs, id, t.TempDir())
	fs.data[id].TmuxSession = id
}

// seatTwo joins two distinct-project agents into one group and returns nothing —
// both a1 and a2 hold seats in "team".
func seatTwo(t *testing.T, srv *Server, fs *fakeStore) {
	t.Helper()
	seedTmux(t, fs, "a1")
	seedTmux(t, fs, "a2")
	if _, err := srv.JoinGroup(context.Background(), joinReq("team", "a1")); err != nil {
		t.Fatalf("join a1: %v", err)
	}
	if _, err := srv.JoinGroup(context.Background(), joinReq("team", "a2")); err != nil {
		t.Fatalf("join a2: %v", err)
	}
}

func TestTerminateGroupedWithoutConfirmIsGated(t *testing.T) {
	srv, fs, fl, mb := newGroupServerWithMbox(t)
	seatTwo(t, srv, fs)

	resp, err := srv.TerminateSession(context.Background(), termReq("a1", false))
	require.NoError(t, err)
	gate, ok := resp.(oapi.TerminateSession409JSONResponse)
	require.True(t, ok, "grouped terminate without confirm must be gated with 409")
	require.NotEmpty(t, gate.Error)
	require.Len(t, gate.Groups, 1)
	require.Equal(t, "team", gate.Groups[0].Name)
	require.Equal(t, []string{"a2"}, gate.Groups[0].Peers)

	// The gate is a refusal: the agent was NOT torn down and its seat stands.
	require.Empty(t, fl.terminated, "gated terminate must not kill tmux")
	require.NotEqual(t, store.StatusDone, fs.data["a1"].Status, "gated terminate must not mark the agent done")
	g, _ := srv.groups.Get("team")
	require.Len(t, g.Members, 2, "gated terminate leaves the roster intact")
	msgs, _ := mb.Messages("a2")
	require.Empty(t, msgs, "no notice until the terminate is actually committed")
}

func TestTerminateGroupedWithConfirmTearsDownAndNotifies(t *testing.T) {
	srv, fs, fl, mb := newGroupServerWithMbox(t)
	seatTwo(t, srv, fs)

	resp, err := srv.TerminateSession(context.Background(), termReq("a1", true))
	require.NoError(t, err)
	_, ok := resp.(oapi.TerminateSession200JSONResponse)
	require.True(t, ok, "confirmed terminate returns 200")

	// Agent is down and its seat is vacated.
	require.Equal(t, "a1", fl.terminated)
	require.Equal(t, store.StatusDone, fs.data["a1"].Status)
	g, _ := srv.groups.Get("team")
	require.Len(t, g.Members, 1)
	require.Equal(t, "a2", g.Members[0].AgentID, "only the terminated agent's seat is removed")

	// The remaining peer got an abandonment notice from warden.
	msgs, err := mb.Messages("a2")
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	require.Equal(t, pipelineWakeSender, msgs[0].From, "notice is a trusted daemon-internal write")
	require.Contains(t, msgs[0].Body, "terminated")
	require.Contains(t, msgs[0].Body, "team")
}

func TestTerminateLegacyPathIgnoresGroups(t *testing.T) {
	// A bodyless terminate (rotate/self-succession) is unconditional and must not
	// touch group seats or notify peers — a successor inherits the work.
	srv, fs, fl, mb := newGroupServerWithMbox(t)
	seatTwo(t, srv, fs)

	resp, err := srv.TerminateSession(context.Background(), legacyTermReq("a1"))
	require.NoError(t, err)
	_, ok := resp.(oapi.TerminateSession200JSONResponse)
	require.True(t, ok)

	require.Equal(t, "a1", fl.terminated, "legacy terminate still kills tmux")
	require.Equal(t, store.StatusDone, fs.data["a1"].Status)
	g, _ := srv.groups.Get("team")
	require.Len(t, g.Members, 2, "legacy terminate leaves the roster untouched")
	msgs, _ := mb.Messages("a2")
	require.Empty(t, msgs, "legacy terminate emits no abandonment notice")
}

func TestTerminateUngroupedWithConfirmIsPlain(t *testing.T) {
	// An ungrouped agent terminates regardless of confirm — the gate only fires
	// for a grouped orchestrator.
	srv, fs, fl, _ := newGroupServerWithMbox(t)
	seedTmux(t, fs, "solo")

	resp, err := srv.TerminateSession(context.Background(), termReq("solo", false))
	require.NoError(t, err)
	_, ok := resp.(oapi.TerminateSession200JSONResponse)
	require.True(t, ok, "ungrouped terminate is never gated")
	require.Equal(t, "solo", fl.terminated)
	require.Equal(t, store.StatusDone, fs.data["solo"].Status)
}

func TestTerminateGroupedSoleMemberNotifiesNoOne(t *testing.T) {
	// A grouped orchestrator with no peers still confirms + tears down, but there
	// is no one to notify.
	srv, fs, _, mb := newGroupServerWithMbox(t)
	seedTmux(t, fs, "a1")
	_, err := srv.JoinGroup(context.Background(), joinReq("solo-team", "a1"))
	require.NoError(t, err)

	// Gate names the group but with an empty peer set.
	resp, err := srv.TerminateSession(context.Background(), termReq("a1", false))
	require.NoError(t, err)
	gate := resp.(oapi.TerminateSession409JSONResponse)
	require.Len(t, gate.Groups, 1)
	require.Empty(t, gate.Groups[0].Peers)

	resp, err = srv.TerminateSession(context.Background(), termReq("a1", true))
	require.NoError(t, err)
	_, ok := resp.(oapi.TerminateSession200JSONResponse)
	require.True(t, ok)
	g, _ := srv.groups.Get("solo-team")
	require.Empty(t, g.Members)
	all, _ := mb.All()
	require.Empty(t, all, "no peers ⇒ no notices")
}

func TestGroupsForAgentSpansMultipleGroups(t *testing.T) {
	srv, fs, _, _ := newGroupServerWithMbox(t)
	seedTmux(t, fs, "a1")
	_, err := srv.JoinGroup(context.Background(), joinReq("g1", "a1"))
	require.NoError(t, err)
	_, err = srv.JoinGroup(context.Background(), joinReq("g2", "a1"))
	require.NoError(t, err)

	grps, err := srv.groupsForAgent("a1")
	require.NoError(t, err)
	names := []string{}
	for _, g := range grps {
		names = append(names, g.Name)
	}
	require.ElementsMatch(t, []string{"g1", "g2"}, names)

	// A confirmed terminate vacates the seat in every group it holds.
	_, err = srv.TerminateSession(context.Background(), termReq("a1", true))
	require.NoError(t, err)
	for _, name := range []string{"g1", "g2"} {
		g, _ := srv.groups.Get(name)
		require.Empty(t, g.Members, "seat vacated in "+name)
	}
}

// ensure the notice text is human-facing and mentions re-delegation guidance.
func TestAbandonmentNoticeText(t *testing.T) {
	srv, fs, _, mb := newGroupServerWithMbox(t)
	seedTmux(t, fs, "orch")
	fs.data["orch"].Name = "backend-orch"
	seedTmux(t, fs, "peer")
	_, err := srv.JoinGroup(context.Background(), joinReq("team", "orch"))
	require.NoError(t, err)
	_, err = srv.JoinGroup(context.Background(), joinReq("team", "peer"))
	require.NoError(t, err)

	_, err = srv.TerminateSession(context.Background(), termReq("orch", true))
	require.NoError(t, err)
	msgs, _ := mb.Messages("peer")
	require.Len(t, msgs, 1)
	require.Contains(t, msgs[0].Body, "backend-orch", "notice names the agent by its friendly name")
	require.True(t, strings.Contains(msgs[0].Body, "abandoned") || strings.Contains(msgs[0].Body, "orphan"))
}
