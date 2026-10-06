package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/planstore"
)

func TestSeedAndMirrorPlanOutcome(t *testing.T) {
	ctx := context.Background()
	plans, err := planstore.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = plans.Close() })

	root := t.TempDir()
	id := planstore.PlanID(root, "demo")
	require.NoError(t, plans.Create(ctx, &planstore.Plan{
		ID: id, ProjectID: root, Name: "demo", Status: planstore.PlanStatusInProgress,
	}))

	srv := &Server{plans: plans}
	srv.seedPlanOutcome(ctx, id, "autopilot/demo", "main")
	p, err := plans.Get(ctx, id)
	require.NoError(t, err)
	require.NotNil(t, p.Outcome)
	require.Equal(t, "autopilot/demo", p.Outcome.IntegrationBranch)
	require.Equal(t, "main", p.Outcome.DefaultBranch)

	srv.mirrorPlanFinalPR(ctx, p, autopilot.RunStatus{
		FinalPR:           &autopilot.FinalPR{Number: 7, URL: "https://x/7", HeadSHA: "abc", State: "open", Gate: "green"},
		IntegrationBranch: "autopilot/demo",
	})
	p, err = plans.Get(ctx, id)
	require.NoError(t, err)
	require.NotNil(t, p.Outcome.FinalPR)
	require.Equal(t, 7, p.Outcome.FinalPR.Number)
	require.Equal(t, "open", p.Outcome.FinalPR.State)
}

func TestDeleteIntegrationBranchMissingIsSuccess(t *testing.T) {
	ctx := context.Background()
	plans, err := planstore.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = plans.Close() })

	repo := t.TempDir()
	initGitRepo(t, repo)
	id := planstore.PlanID(repo, "gone")
	require.NoError(t, plans.Create(ctx, &planstore.Plan{
		ID: id, ProjectID: repo, Name: "gone", Status: planstore.PlanStatusCompleted,
		Outcome: &planstore.PlanOutcome{
			IntegrationBranch: "autopilot/gone",
			DefaultBranch:     "main",
			FinalPR:           &planstore.PlanOutcomeFinalPR{Number: 1, State: "merged", HeadSHA: "deadbeef"},
		},
	}))

	srv := &Server{plans: plans}
	p, err := plans.Get(ctx, id)
	require.NoError(t, err)
	srv.deleteIntegrationBranchAfterMerge(ctx, p)
	p, err = plans.Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, planstore.BranchFateDeleted, p.Outcome.BranchFate)
	require.NotNil(t, p.Outcome.BranchDeletedAt)
}

func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	run := func(args ...string) {
		t.Helper()
		out, err := gitIn(context.Background(), dir, args...)
		require.NoError(t, err, out)
	}
	run("init", "-b", "main")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README"), []byte("x\n"), 0o644))
	run("add", "README")
	run("commit", "-m", "init")
}
