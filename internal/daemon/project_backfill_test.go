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
	alpha, err := projects.OpenProject(alphaDir, "alpha", alphaDir)
	require.NoError(t, err)
	beta, err := projects.OpenProject(betaDir, "beta", betaDir)
	require.NoError(t, err)
	_, err = projects.CloseProject(beta.ID)
	require.NoError(t, err)
	// gamma is closed but has a member via an explicit back-ref (a hibernated
	// member): its lists must still be rebuilt even though it is not path-matchable.
	gammaDir := filepath.Join(dataDir, "gamma")
	gamma, err := projects.OpenProject(gammaDir, "gamma", gammaDir)
	require.NoError(t, err)
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
	require.Equal(t, 2, rep.ProjectsRebuilt, "alpha and the closed gamma gain members")
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
