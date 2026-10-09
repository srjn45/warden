package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/projectstore"
)

type incidentFixture struct {
	Projects []projectstore.Project `json:"projects"`
	Active   []agentstore.Agent     `json:"active"`
	Archived []agentstore.Agent     `json:"archived"`
}

func TestReconcileIdentityConflictsIncidentFixture(t *testing.T) {
	ctx := context.Background()
	raw, err := os.ReadFile(filepath.Join("testdata", "incident_membership.json"))
	require.NoError(t, err)
	var fx incidentFixture
	require.NoError(t, json.Unmarshal(raw, &fx))

	dir := t.TempDir()
	ss, err := agentstore.New(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ss.Close() })
	projects, err := projectstore.NewStore(filepath.Join(dir, "projects"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = projects.Close() })
	for _, p := range fx.Projects {
		require.NoError(t, projects.Upsert(p))
	}
	// Archived first: the same logical id then legitimately re-appears in active,
	// reproducing the incident's active/archive duplicate.
	for i := range fx.Archived {
		a := fx.Archived[i]
		a.Name = "n-arch-" + a.ID
		require.NoError(t, ss.Insert(ctx, &a))
		require.NoError(t, ss.Archive(ctx, a.ID))
	}
	for i := range fx.Active {
		a := fx.Active[i]
		a.Name = "n-" + a.ID
		require.NoError(t, ss.Insert(ctx, &a))
	}
	before := map[string]string{}
	for _, id := range []string{"agent-dup", "agent-shared", "agent-1", "agent-removed"} {
		a, err := ss.Get(ctx, id)
		require.NoError(t, err)
		before[id] = a.ProjectID
	}

	rep, err := ReconcileProjectMembership(ctx, ss, nil, nil, projects)
	require.NoError(t, err)

	kinds := map[string]string{}
	for _, c := range rep.Conflicts {
		kinds[c.ID] = c.Kind
	}
	require.Equal(t, ConflictDuplicateAgentID, kinds["agent-dup"])
	require.Equal(t, ConflictAmbiguousMembership, kinds["agent-shared"])
	require.NotContains(t, kinds, "agent-1")
	require.NotContains(t, kinds, "agent-gone", "dangling membership is tolerated")
	require.NotContains(t, kinds, "agent-removed", "explicit removal is not a conflict")

	// Conflicted agents are never stamped; the verified one is.
	for _, id := range []string{"agent-dup", "agent-shared"} {
		a, err := ss.Get(ctx, id)
		require.NoError(t, err)
		require.Equal(t, before[id], a.ProjectID, id)
	}
	a1, err := ss.Get(ctx, "agent-1")
	require.NoError(t, err)
	require.Equal(t, "proj-a", a1.ProjectID)
	// proj-c's authoritative [] is an explicit removal: reverse edge cleared.
	rm, err := ss.Get(ctx, "agent-removed")
	require.NoError(t, err)
	require.Equal(t, "", rm.ProjectID)

	// Dangling member and forward lists survive untouched.
	pa, err := projects.Get("proj-a")
	require.NoError(t, err)
	require.Equal(t, []string{"agent-1", "agent-dup", "agent-shared", "agent-gone"}, pa.Agents)

	// Archive is read-only for reconcile; second run is a no-op.
	closed, err := ss.ListClosed(ctx)
	require.NoError(t, err)
	require.Len(t, closed, 1)
	require.Equal(t, "proj-a", closed[0].ProjectID)
	rep2, err := ReconcileProjectMembership(ctx, ss, nil, nil, projects)
	require.NoError(t, err)
	require.False(t, rep2.Changed())
}

func TestDetectIdentityConflictsWithinArchive(t *testing.T) {
	got := detectIdentityConflicts(nil, []*agentstore.Agent{{ID: "x"}, {ID: "x"}}, nil)
	require.Len(t, got, 1)
	require.Equal(t, ConflictDuplicateAgentID, got[0].Kind)
}
