package daemon

import (
	"context"
	"testing"

	"github.com/srjn45/warden/internal/projectstore"
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
	fs.closed["gone"] = &store.Session{ID: "gone", TmuxSession: "gone", Status: store.StatusOrphaned}
	alive := func(context.Context, string) bool { return false }

	results, err := recoverCandidates(context.Background(), fs, alive, true)
	require.NoError(t, err)
	require.Empty(t, results)
	require.NotContains(t, fs.data, "gone")
}

// Spec D8: a live tmux pane on a non-orphaned archived record is not recovery
// candidate material — only orphaned archives may be revived.
func TestRecoverCandidatesSkipsNonOrphaned(t *testing.T) {
	fs := newFakeStore()
	fs.closed["done"] = &store.Session{ID: "done", TmuxSession: "done", Status: store.StatusDone}
	fs.closed["idle"] = &store.Session{ID: "idle", TmuxSession: "idle", Status: store.StatusIdle}
	alive := func(context.Context, string) bool { return true }

	results, err := recoverCandidates(context.Background(), fs, alive, true)
	require.NoError(t, err)
	require.Empty(t, results)
	require.NotContains(t, fs.data, "done")
	require.NotContains(t, fs.data, "idle")
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

// TestRecoverApplyRestoresMembershipAndChildEdge proves Archive's delete-side
// edge removals are mirrored on successful recover apply: Project.agents[] and
// parent.ChildAgents[] are restored from the re-inserted back-refs (spec §6.1).
// Eligibility gating (D8) is unchanged — this only covers the post-insert wrapper.
func TestRecoverApplyRestoresMembershipAndChildEdge(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	ps, err := projectstore.NewStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { ps.Close() })

	proj, err := ps.OpenProject("/projects/alpha", "alpha", "/projects/alpha")
	require.NoError(t, err)

	parent := &store.Session{ID: "agent-parent", Status: store.StatusWorking, ProjectID: proj.ID}
	require.NoError(t, fs.Insert(ctx, parent))
	// Simulate a prior spawn that stamped both ends, then Archive that dropped
	// the forward edges while the orphaned record retained its back-refs.
	_, err = ps.AddAgentToProject(proj.ID, "agent-parent")
	require.NoError(t, err)

	archived := &store.Session{
		ID: "agent-child", TmuxSession: "agent-child", Status: store.StatusOrphaned,
		ParentID: parent.ID, ProjectID: proj.ID, Name: "child",
	}
	fs.closed["agent-child"] = archived
	// Forward edges empty — as after removeProjectMembership/removeChildEdge on Archive.
	gotProj, err := ps.Get(proj.ID)
	require.NoError(t, err)
	require.NotContains(t, gotProj.Agents, "agent-child")
	require.Nil(t, parent.ChildAgents)

	s := &Server{store: fs, projects: ps}
	alive := func(context.Context, string) bool { return true }
	results, err := recoverCandidates(ctx, fs, alive, true)
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.True(t, results[0].Recovered)

	s.restoreRecoveredEdges(ctx, results)

	gotProj, err = ps.Get(proj.ID)
	require.NoError(t, err)
	require.Contains(t, gotProj.Agents, "agent-child", "Project.agents[] restored after recover")

	gotParent, err := fs.Get(ctx, "agent-parent")
	require.NoError(t, err)
	require.Contains(t, gotParent.ChildAgents, "agent-child", "parent.ChildAgents[] restored after recover")

	gotChild, err := fs.Get(ctx, "agent-child")
	require.NoError(t, err)
	require.Equal(t, "agent-parent", gotChild.ParentID, "back-ref preserved on re-insert")
	require.Equal(t, proj.ID, gotChild.ProjectID)

	// Idempotent: a second restore does not duplicate.
	s.restoreRecoveredEdges(ctx, results)
	gotProj, err = ps.Get(proj.ID)
	require.NoError(t, err)
	require.Equal(t, 1, countID(gotProj.Agents, "agent-child"))
	gotParent, err = fs.Get(ctx, "agent-parent")
	require.NoError(t, err)
	require.Equal(t, 1, countID(gotParent.ChildAgents, "agent-child"))
}

func countID(list []string, id string) int {
	n := 0
	for _, x := range list {
		if x == id {
			n++
		}
	}
	return n
}
