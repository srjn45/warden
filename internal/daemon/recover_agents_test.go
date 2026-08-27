package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/groupstore"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// A closed record whose tmux session is confirmed alive is a dry-run
// candidate and, when re-run with apply, is re-inserted into the active
// store under its original id with its metadata intact.
func TestRecoverCandidatesDryRunThenApply(t *testing.T) {
	fs := newFakeStore()
	fs.closed["orch"] = &store.Session{
		ID: "orch", TmuxSession: "orch", Workdir: "/repo", Name: "orchestrator",
		Subject: "doing things", ParentID: "", Status: store.StatusOrphaned,
	}
	alive := func(context.Context, string) bool { return true }

	dry, err := recoverCandidates(context.Background(), fs, alive, false)
	require.NoError(t, err)
	require.Len(t, dry, 1)
	require.Equal(t, "orch", dry[0].ID)
	require.False(t, dry[0].Recovered, "dry run must not mutate anything")
	require.NotContains(t, fs.data, "orch", "dry run changes nothing")

	applied, err := recoverCandidates(context.Background(), fs, alive, true)
	require.NoError(t, err)
	require.Len(t, applied, 1)
	require.True(t, applied[0].Recovered)
	require.Empty(t, applied[0].Error)
	require.Contains(t, fs.data, "orch", "apply re-inserts the record")
	require.Equal(t, store.StatusWorking, fs.data["orch"].Status, "revived to a live status, not left orphaned")
	require.Equal(t, "orchestrator", fs.data["orch"].Name, "original metadata is preserved")
}

// A closed record whose tmux session is confirmed dead is not a candidate —
// it was archived correctly and recover must not resurrect it.
func TestRecoverCandidatesSkipsGenuinelyDead(t *testing.T) {
	fs := newFakeStore()
	fs.closed["gone"] = &store.Session{ID: "gone", TmuxSession: "gone", Status: store.StatusDone}
	alive := func(context.Context, string) bool { return false }

	results, err := recoverCandidates(context.Background(), fs, alive, true)
	require.NoError(t, err)
	require.Empty(t, results)
	require.NotContains(t, fs.data, "gone")
}

// A nil alive func (no liveness checker wired) yields zero candidates —
// recover must never guess at liveness.
func TestRecoverCandidatesNoCheckerYieldsNone(t *testing.T) {
	fs := newFakeStore()
	fs.closed["orch"] = &store.Session{ID: "orch", TmuxSession: "orch", Status: store.StatusOrphaned}

	results, err := recoverCandidates(context.Background(), fs, nil, false)
	require.NoError(t, err)
	require.Empty(t, results)
}

// An id already active (e.g. a previous recover run already reinstated it)
// is not re-offered as a candidate.
func TestRecoverCandidatesSkipsAlreadyActive(t *testing.T) {
	fs := newFakeStore()
	fs.data["orch"] = &store.Session{ID: "orch", TmuxSession: "orch", Status: store.StatusWorking}
	fs.closed["orch"] = &store.Session{ID: "orch", TmuxSession: "orch", Status: store.StatusOrphaned}
	alive := func(context.Context, string) bool { return true }

	results, err := recoverCandidates(context.Background(), fs, alive, false)
	require.NoError(t, err)
	require.Empty(t, results)
}

// TestRecoverRejoinGroupsReannouncesPeers is the B7 acceptance: after an
// orchestrator's session is recovered, rejoinGroups refreshes its JoinedAt
// timestamp and delivers a "re-joined after recovery" notice to its group
// peers. The recovered agent itself is NOT re-sent the roster.
func TestRecoverRejoinGroupsReannouncesPeers(t *testing.T) {
	ctx := context.Background()
	srv, fs, _, mb := newGroupServerMbox(t)

	// Seed group "team" with two members: orch (will be recovered) and peer.
	oldTime := time.Now().Add(-2 * time.Hour).UTC()
	if err := srv.groups.Create(&groupstore.Group{
		Name: "team",
		Members: []groupstore.Member{
			{AgentID: "orch", ProjectKey: "github.com/org/be", JoinedAt: oldTime},
			{AgentID: "peer", ProjectKey: "github.com/org/fe", JoinedAt: oldTime},
		},
	}); err != nil {
		t.Fatalf("create group: %v", err)
	}

	// peer is live in the active store; orch is archived (simulates a restart).
	wd := t.TempDir()
	require.NoError(t, fs.Insert(ctx, &store.Session{
		ID: "peer", Workdir: wd, Status: store.StatusWorking,
	}))
	fs.closed["orch"] = &store.Session{
		ID: "orch", TmuxSession: "orch-pane", Workdir: wd,
		Name: "be-orchestrator", Status: store.StatusOrphaned,
	}

	// Run recoverCandidates directly (alive=true) to re-insert orch.
	alive := func(context.Context, string) bool { return true }
	results, err := recoverCandidates(ctx, fs, alive, true)
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.True(t, results[0].Recovered)

	// Collect recovered sessions and call rejoinGroups (mirrors what Recover does).
	var recovered []*store.Session
	for _, r := range results {
		if !r.Recovered {
			continue
		}
		sess, serr := fs.Get(ctx, r.ID)
		require.NoError(t, serr)
		recovered = append(recovered, sess)
	}
	srv.rejoinGroups(ctx, recovered)

	// peer must have received exactly one "re-joined after recovery" notice.
	peerMsgs, err := mb.Messages("peer")
	require.NoError(t, err)
	require.Len(t, peerMsgs, 1)
	require.Contains(t, peerMsgs[0].Body, "re-joined after recovery",
		"re-announce uses the recovery-specific verb")
	require.True(t,
		strings.Contains(peerMsgs[0].Body, "orch") ||
			strings.Contains(peerMsgs[0].Body, "be-orchestrator"),
		"re-announce identifies the recovered agent")

	// orch must NOT receive a roster (it already has that from the initial join).
	orchMsgs, err := mb.Messages("orch")
	require.NoError(t, err)
	require.Empty(t, orchMsgs, "recovered agent is not re-sent the roster")

	// orch's JoinedAt must be refreshed to a time after oldTime.
	grp, err := srv.groups.Get("team")
	require.NoError(t, err)
	var orchMem groupstore.Member
	for _, m := range grp.Members {
		if m.AgentID == "orch" {
			orchMem = m
			break
		}
	}
	require.NotEmpty(t, orchMem.AgentID, "orch must still be a member")
	require.True(t, orchMem.JoinedAt.After(oldTime),
		"JoinedAt must be refreshed after recovery; got %v, oldTime %v", orchMem.JoinedAt, oldTime)
}

// TestRecoverRejoinGroupsNoGroupStoreIsNoop confirms rejoinGroups is safe when
// no group store is configured (nil s.groups).
func TestRecoverRejoinGroupsNoGroupStoreIsNoop(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	srv := &Server{store: fs, hub: newHub(), done: make(chan struct{})}
	// groups is nil — rejoinGroups must not panic.
	sess := &store.Session{ID: "orch", Status: store.StatusWorking}
	require.NotPanics(t, func() { srv.rejoinGroups(ctx, []*store.Session{sess}) })
}

// TestRecoverRejoinGroupsNoMboxIsNoop confirms rejoinGroups is safe when the
// mailbox is not configured (nil s.mbox) — re-announces are skipped silently.
func TestRecoverRejoinGroupsNoMboxIsNoop(t *testing.T) {
	ctx := context.Background()
	srv, fs, _ := newGroupServer(t) // no mbox wired
	wd := t.TempDir()

	if err := srv.groups.Create(&groupstore.Group{
		Name: "team",
		Members: []groupstore.Member{
			{AgentID: "orch", ProjectKey: "github.com/org/be", JoinedAt: time.Now().Add(-time.Hour)},
			{AgentID: "peer", ProjectKey: "github.com/org/fe", JoinedAt: time.Now().Add(-time.Hour)},
		},
	}); err != nil {
		t.Fatalf("create group: %v", err)
	}
	require.NoError(t, fs.Insert(ctx, &store.Session{ID: "peer", Workdir: wd, Status: store.StatusWorking}))

	sess := &store.Session{ID: "orch", Workdir: wd, Status: store.StatusWorking}
	require.NotPanics(t, func() { srv.rejoinGroups(ctx, []*store.Session{sess}) })

	// JoinedAt must still have been refreshed (the group store update happens
	// regardless of the mailbox being nil).
	grp, err := srv.groups.Get("team")
	require.NoError(t, err)
	for _, m := range grp.Members {
		if m.AgentID == "orch" {
			require.True(t, time.Since(m.JoinedAt) < time.Minute,
				"JoinedAt refreshed even without mbox")
		}
	}
}

// A failed re-insert (e.g. a name collision) is reported per-candidate
// without aborting the rest of the batch.
func TestRecoverCandidatesReportsInsertError(t *testing.T) {
	fs := newFakeStore()
	fs.closed["orch"] = &store.Session{ID: "orch", TmuxSession: "orch", Status: store.StatusOrphaned}
	fs.insertErr = store.ErrExists
	alive := func(context.Context, string) bool { return true }

	results, err := recoverCandidates(context.Background(), fs, alive, true)
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.False(t, results[0].Recovered)
	require.NotEmpty(t, results[0].Error)
}
