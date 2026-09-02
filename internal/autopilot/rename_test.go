package autopilot

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRenamePreservesIntegrationBranch(t *testing.T) {
	repo := t.TempDir()
	plan := writePlan(t, repo, "ship.yaml", "ship it")
	c := NewController(ControllerConfig{DataDir: t.TempDir(), BaseDir: repo}, &fakeEnv{})
	t.Cleanup(func() { require.NoError(t, c.Close()) })

	st, err := c.Register(context.Background(), RegisterRequest{Name: "ship", PlanFile: plan})
	require.NoError(t, err)
	oldBranch := c.runs[st.RunID].integrationBranch
	require.Equal(t, "autopilot/ship", oldBranch)

	renamed, err := c.RenameRun(context.Background(), st.RunID, "voyage")
	require.NoError(t, err)
	require.Equal(t, "voyage", renamed.Name)
	require.Equal(t, oldBranch, c.runs[st.RunID].integrationBranch)

	lp, ok := c.LandParams(st.RunID)
	require.True(t, ok)
	require.Equal(t, oldBranch, lp.IntegrationBranch)

	rec, err := c.store.Get(st.RunID)
	require.NoError(t, err)
	require.Equal(t, oldBranch, rec.IntegrationBranch)
}

func TestRetargetIntegrationBranchExplicit(t *testing.T) {
	repo := t.TempDir()
	plan := writePlan(t, repo, "ship.yaml", "ship it")
	c := NewController(ControllerConfig{DataDir: t.TempDir(), BaseDir: repo}, &fakeEnv{})
	t.Cleanup(func() { require.NoError(t, c.Close()) })

	st, err := c.Register(context.Background(), RegisterRequest{Name: "ship", PlanFile: plan})
	require.NoError(t, err)

	retargeted, err := c.RetargetIntegrationBranch(context.Background(), st.RunID, RetargetIntegrationBranchRequest{
		IntegrationBranch: "autopilot/voyage",
	})
	require.NoError(t, err)
	require.Equal(t, "ship", retargeted.Name)
	require.Equal(t, "autopilot/voyage", c.runs[st.RunID].integrationBranch)

	lp, ok := c.LandParams(st.RunID)
	require.True(t, ok)
	require.Equal(t, "autopilot/voyage", lp.IntegrationBranch)
}

func TestRetargetIntegrationBranchDeriveAfterRename(t *testing.T) {
	repo := t.TempDir()
	plan := writePlan(t, repo, "ship.yaml", "ship it")
	c := NewController(ControllerConfig{DataDir: t.TempDir(), BaseDir: repo}, &fakeEnv{})
	t.Cleanup(func() { require.NoError(t, c.Close()) })

	st, err := c.Register(context.Background(), RegisterRequest{Name: "ship", PlanFile: plan})
	require.NoError(t, err)
	_, err = c.RenameRun(context.Background(), st.RunID, "voyage")
	require.NoError(t, err)

	retargeted, err := c.RetargetIntegrationBranch(context.Background(), st.RunID, RetargetIntegrationBranchRequest{Derive: true})
	require.NoError(t, err)
	require.Equal(t, "voyage", retargeted.Name)
	require.Equal(t, "autopilot/voyage", c.runs[st.RunID].integrationBranch)
}

func TestRetargetRejectsActiveRun(t *testing.T) {
	repo := t.TempDir()
	plan := writePlan(t, repo, "ship.yaml", "ship it")
	c := NewController(ControllerConfig{DataDir: t.TempDir(), BaseDir: repo, Resolver: &fakeResolver{backendID: "a", tier: "free"}}, &fakeEnv{})
	c.SetRuntime(newFakeRuntime())
	t.Cleanup(func() { require.NoError(t, c.Close()) })

	st, err := c.Register(context.Background(), RegisterRequest{Name: "ship", PlanFile: plan})
	require.NoError(t, err)
	_, err = c.StartRun(context.Background(), st.RunID)
	require.NoError(t, err)

	_, err = c.RetargetIntegrationBranch(context.Background(), st.RunID, RetargetIntegrationBranchRequest{
		IntegrationBranch: "autopilot/other",
	})
	require.ErrorIs(t, err, ErrRunConflict)
}

func TestLandAfterRenameWithoutRetargetUsesStoredBranch(t *testing.T) {
	repo := t.TempDir()
	plan := writePlan(t, repo, "ship.yaml", "ship it")
	c := NewController(ControllerConfig{DataDir: t.TempDir(), BaseDir: repo}, &fakeEnv{})
	t.Cleanup(func() { require.NoError(t, c.Close()) })

	st, err := c.Register(context.Background(), RegisterRequest{Name: "ship", PlanFile: plan})
	require.NoError(t, err)
	_, err = c.RenameRun(context.Background(), st.RunID, "voyage")
	require.NoError(t, err)

	_, err = Land(context.Background(), LandRequest{
		RunActive:         true,
		Owned:             true,
		Branch:            "worker-1",
		IntegrationBranch: c.runs[st.RunID].integrationBranch,
		DefaultBranch:     "main",
		Gate:              "local",
		Strategy:          "squash",
	}, landHostStub{pr: PRInfo{Number: 1, BaseRef: "autopilot/voyage", HeadSHA: "abc", Mergeable: true}}, nil)
	require.Error(t, err)
	var le *LandError
	require.ErrorAs(t, err, &le)
	require.Equal(t, ErrWrongBase, le.Kind)
}

type landHostStub struct{ pr PRInfo }

func (h landHostStub) FindPR(context.Context, string) (PRInfo, bool, error) {
	return h.pr, true, nil
}
func (landHostStub) GateCI(context.Context, string, string, string) (GateState, string, error) {
	return GateGreen, "", nil
}
func (landHostStub) GateLocal(context.Context, string) (GateState, string, error) {
	return GateGreen, "", nil
}
func (landHostStub) Merge(context.Context, int, string, bool) (string, error) { return "sha", nil }
