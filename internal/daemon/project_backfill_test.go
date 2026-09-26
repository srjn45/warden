package daemon

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/projectstore"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// TestReconcileProjectMembershipFixtureDB drives the backfill/reconcile against
// real on-disk fixture stores (session FileStore, pipeline + project ScrivaDB) and
// asserts both the stamping + rebuild outcome and idempotency on a second run.
func TestReconcileProjectMembershipFixtureDB(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()

	sstore, err := store.NewFileStore(dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sstore.Close(ctx) })

	pstore, err := pipeline.NewStore(filepath.Join(dataDir, "pipelines"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = pstore.Close() })

	projects, err := projectstore.NewStore(filepath.Join(dataDir, "projects"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = projects.Close() })

	// Two projects: alpha is open (path-matchable), beta is closed (never matched).
	alphaDir := filepath.Join(dataDir, "alpha")
	betaDir := filepath.Join(dataDir, "beta")
	alpha := projectstore.Project{ID: alphaDir, Name: "alpha", Path: alphaDir}
	require.NoError(t, projects.Upsert(alpha))
	beta := projectstore.Project{ID: betaDir, Name: "beta", Path: betaDir}
	require.NoError(t, projects.Upsert(beta))
	_, err = projects.CloseProject(beta.ID)
	require.NoError(t, err)
	// gamma is closed but has a member via an explicit back-ref (a hibernated
	// member): its lists must still be rebuilt even though it is not path-matchable.
	gammaDir := filepath.Join(dataDir, "gamma")
	gamma := projectstore.Project{ID: gammaDir, Name: "gamma", Path: gammaDir}
	require.NoError(t, projects.Upsert(gamma))
	_, err = projects.CloseProject(gamma.ID)
	require.NoError(t, err)

	// Sessions.
	insertSession(t, ctx, sstore, &store.Session{ID: "agent-a1", Repo: alphaDir})                               // stamp → alpha agent
	insertSession(t, ctx, sstore, &store.Session{ID: "agent-a2", ProjectID: alpha.ID})                          // already member
	insertSession(t, ctx, sstore, &store.Session{ID: "term-t1", Kind: store.KindTerminal, ProjectID: alpha.ID}) // terminal member
	insertSession(t, ctx, sstore, &store.Session{ID: "agent-b1", Repo: betaDir})                                // beta closed → not stamped
	insertSession(t, ctx, sstore, &store.Session{ID: "agent-x1", Repo: filepath.Join(dataDir, "unknown")})      // no project → not stamped
	insertSession(t, ctx, sstore, &store.Session{ID: "agent-g1", ProjectID: gamma.ID})                          // hibernated member of a closed project

	// Pipelines.
	require.NoError(t, pstore.Create(&pipeline.Pipeline{ID: "pipe-1", Name: "pipe-1", Repo: alphaDir}))      // stamp → alpha
	require.NoError(t, pstore.Create(&pipeline.Pipeline{ID: "pipe-2", Name: "pipe-2", ProjectID: alpha.ID})) // already member
	require.NoError(t, pstore.Create(&pipeline.Pipeline{ID: "pipe-b", Name: "pipe-b", Repo: betaDir}))       // beta closed → not stamped

	// First run: stamps the two project-less rows that path-match alpha, and rebuilds
	// alpha's lists. beta and the unknown-dir rows are left project-less and unlisted.
	rep, err := ReconcileProjectMembership(ctx, sstore, pstore, projects)
	require.NoError(t, err)
	require.Equal(t, 1, rep.SessionsStamped, "only agent-a1 path-matches an open project")
	require.Equal(t, 1, rep.PipelinesStamped, "only pipe-1 path-matches an open project")
	require.Equal(t, 3, rep.ProjectsRebuilt, "all legacy lists become authoritative, including empty beta")
	require.True(t, rep.Changed())

	// Back-refs stamped for open-project matches only.
	require.Equal(t, alpha.ID, getSession(t, ctx, sstore, "agent-a1").ProjectID)
	require.Empty(t, getSession(t, ctx, sstore, "agent-b1").ProjectID, "closed project is never auto-matched")
	require.Empty(t, getSession(t, ctx, sstore, "agent-x1").ProjectID, "no matching project")
	p1, err := pstore.Get("pipe-1")
	require.NoError(t, err)
	require.Equal(t, alpha.ID, p1.ProjectID)
	pb, err := pstore.Get("pipe-b")
	require.NoError(t, err)
	require.Empty(t, pb.ProjectID, "closed project is never auto-matched")

	// alpha's authoritative lists, rebuilt from back-refs (sorted + de-duplicated).
	gotAlpha, err := projects.Get(alpha.ID)
	require.NoError(t, err)
	require.Equal(t, []string{"agent-a1", "agent-a2"}, gotAlpha.Agents)
	require.Equal(t, []string{"term-t1"}, gotAlpha.Terminals)
	require.Equal(t, []string{"pipe-1", "pipe-2"}, gotAlpha.Pipelines)

	// beta stays empty (its would-be members were never stamped to it).
	gotBeta, err := projects.Get(beta.ID)
	require.NoError(t, err)
	require.Empty(t, gotBeta.Agents)
	require.Empty(t, gotBeta.Terminals)
	require.Empty(t, gotBeta.Pipelines)

	// gamma (closed) is rebuilt from its explicit back-ref member.
	gotGamma, err := projects.Get(gamma.ID)
	require.NoError(t, err)
	require.Equal(t, []string{"agent-g1"}, gotGamma.Agents)

	// Second run over the now-reconciled store is a no-op (idempotent).
	rep2, err := ReconcileProjectMembership(ctx, sstore, pstore, projects)
	require.NoError(t, err)
	require.Equal(t, MembershipReconcileReport{}, rep2, "reconcile must be idempotent")
	require.False(t, rep2.Changed())

	// Lists unchanged after the second run.
	gotAlpha2, err := projects.Get(alpha.ID)
	require.NoError(t, err)
	require.Equal(t, gotAlpha.Agents, gotAlpha2.Agents)
	require.Equal(t, gotAlpha.Terminals, gotAlpha2.Terminals)
	require.Equal(t, gotAlpha.Pipelines, gotAlpha2.Pipelines)
}

// TestReconcileProjectMembershipNilStores tolerates unconfigured stores.
func TestReconcileProjectMembershipNilStores(t *testing.T) {
	ctx := context.Background()

	// Nil projects store: no-op, no error.
	rep, err := ReconcileProjectMembership(ctx, nil, nil, nil)
	require.NoError(t, err)
	require.False(t, rep.Changed())

	// Projects but no sessions/pipelines stores: rebuild runs, finds no members.
	projects, err := projectstore.NewStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = projects.Close() })
	_, err = projects.OpenProject("/projects/solo", "solo", "/projects/solo")
	require.NoError(t, err)

	rep, err = ReconcileProjectMembership(ctx, nil, nil, projects)
	require.NoError(t, err)
	require.False(t, rep.Changed())
}

func insertSession(t *testing.T, ctx context.Context, s store.Store, sess *store.Session) {
	t.Helper()
	require.NoError(t, s.Insert(ctx, sess))
}

func getSession(t *testing.T, ctx context.Context, s store.Store, id string) *store.Session {
	t.Helper()
	got, err := s.Get(ctx, id)
	require.NoError(t, err)
	return got
}

func TestReconcilePreservesForwardAuthority(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	ss, err := store.NewFileStore(dir)
	require.NoError(t, err)
	defer ss.Close(ctx)
	ps, err := pipeline.NewStore(filepath.Join(dir, "pipelines"))
	require.NoError(t, err)
	defer ps.Close()
	projects, err := projectstore.NewStore(filepath.Join(dir, "projects"))
	require.NoError(t, err)
	defer projects.Close()
	// Name sort deliberately disagrees with ID sort. Duplicate forward claims
	// retain their lists while the reverse edge chooses the lowest project ID.
	a := projectstore.Project{ID: "a", Name: "Z", Status: projectstore.StatusClosed, Agents: []string{"z", "missing", "a1", "shared"}, Terminals: []string{"missing-t", "t"}, Pipelines: []string{"z-p", "missing-p", "p", "shared-p"}}
	b := projectstore.Project{ID: "b", Name: "A", Agents: []string{"shared"}, Pipelines: []string{"shared-p"}, Terminals: []string{}}
	empty := projectstore.Project{ID: "empty", Path: "/empty", Agents: []string{}, Terminals: []string{}, Pipelines: []string{}}
	// Only the missing pipelines list may be backfilled on this mixed row.
	mixed := projectstore.Project{ID: "mixed", Agents: []string{"dangling"}, Terminals: []string{}}
	for _, p := range []projectstore.Project{a, b, empty, mixed} {
		require.NoError(t, projects.Upsert(p))
	}
	for _, s := range []*store.Session{
		{ID: "z", ProjectID: "b"}, {ID: "a1"}, {ID: "shared", ProjectID: "b"},
		{ID: "t", Kind: store.KindTerminal, ProjectID: "b"},
		{ID: "excluded", ProjectID: "empty", Repo: "/empty"}, {ID: "path-only", Repo: "/empty"},
		{ID: "excluded-terminal", Kind: store.KindTerminal, ProjectID: "empty"},
		{ID: "excluded-mixed", ProjectID: "mixed"},
	} {
		insertSession(t, ctx, ss, s)
	}
	for _, p := range []*pipeline.Pipeline{
		{ID: "z-p", ProjectID: "b"}, {ID: "p"}, {ID: "shared-p", ProjectID: "b"},
		{ID: "excluded-p", ProjectID: "empty", Repo: "/empty"}, {ID: "path-p", Repo: "/empty"},
		{ID: "mixed-p", ProjectID: "mixed"},
	} {
		require.NoError(t, ps.Create(p))
	}
	rep, err := ReconcileProjectMembership(ctx, ss, ps, projects)
	require.NoError(t, err)
	require.True(t, rep.Changed())
	require.Equal(t, 1, rep.ProjectsRebuilt)
	for _, want := range []projectstore.Project{a, b, empty} {
		got, err := projects.Get(want.ID)
		require.NoError(t, err)
		require.Equal(t, want.Agents, got.Agents)
		require.Equal(t, want.Pipelines, got.Pipelines)
		require.Equal(t, want.Terminals, got.Terminals)
	}
	got, err := projects.Get("mixed")
	require.NoError(t, err)
	require.Equal(t, []string{"mixed-p"}, got.Pipelines)
	require.Equal(t, mixed.Agents, got.Agents)
	require.Equal(t, mixed.Terminals, got.Terminals)
	for _, id := range []string{"z", "a1", "shared", "t"} {
		require.Equal(t, "a", getSession(t, ctx, ss, id).ProjectID)
	}
	for _, id := range []string{"excluded", "path-only", "excluded-terminal", "excluded-mixed"} {
		require.Empty(t, getSession(t, ctx, ss, id).ProjectID)
	}
	for _, id := range []string{"z-p", "p", "shared-p"} {
		p, err := ps.Get(id)
		require.NoError(t, err)
		require.Equal(t, "a", p.ProjectID)
	}
	for _, id := range []string{"excluded-p", "path-p"} {
		p, err := ps.Get(id)
		require.NoError(t, err)
		require.Empty(t, p.ProjectID)
	}
	rep, err = ReconcileProjectMembership(ctx, ss, ps, projects)
	require.NoError(t, err)
	require.Equal(t, MembershipReconcileReport{}, rep)
}

func TestReconcileUnavailableStoresRetainLegacyLists(t *testing.T) {
	projects, err := projectstore.NewStore(t.TempDir())
	require.NoError(t, err)
	defer projects.Close()
	require.NoError(t, projects.Upsert(projectstore.Project{ID: "legacy"}))
	rep, err := ReconcileProjectMembership(context.Background(), nil, nil, projects)
	require.NoError(t, err)
	require.False(t, rep.Changed())
	p, err := projects.Get("legacy")
	require.NoError(t, err)
	require.Nil(t, p.Agents)
	require.Nil(t, p.Pipelines)
	require.Nil(t, p.Terminals)
}

func TestChildLastRemovalRetainsAuthorityInDB(t *testing.T) {
	ctx := context.Background()
	ss, err := store.NewFileStore(t.TempDir())
	require.NoError(t, err)
	defer ss.Close(ctx)
	insertSession(t, ctx, ss, &store.Session{ID: "parent", ChildAgents: []string{"child"}, ChildPipelines: []string{"pipe"}})
	s := &Server{store: ss}
	s.removeChildEdge(ctx, &store.Session{ID: "child", ParentID: "parent"})
	s.removePipelineParentEdge(ctx, &pipeline.Pipeline{ID: "pipe", ParentAgentID: "parent"})
	got := getSession(t, ctx, ss, "parent")
	require.Equal(t, []string{}, got.ChildAgents)
	require.Equal(t, []string{}, got.ChildPipelines)
}
