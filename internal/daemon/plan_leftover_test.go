package daemon

import (
	"context"
	"os/exec"
	"testing"

	"github.com/srjn45/warden/internal/planstore"
	"github.com/stretchr/testify/require"
)

func TestIntegrationLeftover(t *testing.T) {
	repo := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput()
		require.NoError(t, err, string(out))
	}
	run("init", "-b", "main")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	run("commit", "--allow-empty", "-m", "base")
	run("update-ref", "refs/remotes/origin/main", "HEAD")
	run("checkout", "-b", "autopilot/flow")
	run("commit", "--allow-empty", "-m", "one")
	run("commit", "--allow-empty", "-m", "two")
	run("checkout", "main")

	s := &Server{}
	done := func(p planstore.Plan) *planstore.Plan {
		p.ProjectID, p.Name, p.Status = repo, "flow", planstore.PlanStatusCompleted
		return &p
	}
	ctx := context.Background()

	lo := s.integrationLeftover(ctx, done(planstore.Plan{ExecutionMode: planstore.PlanModeAutopilot}))
	require.NotNil(t, lo)
	require.Equal(t, "autopilot/flow", lo.branch)
	require.Equal(t, 2, lo.commits)

	// A recorded fate means nothing is computed.
	require.Nil(t, s.integrationLeftover(ctx, done(planstore.Plan{
		ExecutionMode: planstore.PlanModeAutopilot,
		Outcome:       &planstore.PlanOutcome{BranchFate: planstore.BranchFateDeleted},
	})))
	// Pipeline plans and in-progress plans are never flagged.
	require.Nil(t, s.integrationLeftover(ctx, done(planstore.Plan{ExecutionMode: planstore.PlanModePipeline})))
	p := done(planstore.Plan{ExecutionMode: planstore.PlanModeAutopilot})
	p.Status = planstore.PlanStatusInProgress
	require.Nil(t, s.integrationLeftover(ctx, p))

	// Branch fully merged into default → nothing leftover.
	run("update-ref", "refs/remotes/origin/main", "autopilot/flow")
	require.Nil(t, s.integrationLeftover(ctx, done(planstore.Plan{ExecutionMode: planstore.PlanModeAutopilot})))

	got := leftoverOutcome(nil, &planLeftover{branch: "b", def: "main", commits: 1})
	require.Equal(t, planstore.BranchFateLeftoverUnmerged, got.BranchFate)
}
