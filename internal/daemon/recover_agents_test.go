package daemon

import (
	"context"
	"testing"

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
