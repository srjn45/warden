package autopilot

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSlotScopeDistinctAcrossReposWithSamePlanName(t *testing.T) {
	repoA := t.TempDir()
	repoB := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(repoA, "plans"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(repoB, "plans"), 0o755))
	planA := writePlan(t, repoA, "plans/default.yaml", "repo a")
	planB := writePlan(t, repoB, "plans/default.yaml", "repo b")

	env := &fakeEnv{repoOf: func(dir string) (string, error) {
		dir = filepath.Clean(dir)
		if strings.HasPrefix(dir, repoA) {
			return repoA, nil
		}
		return repoB, nil
	}}
	c := NewController(ControllerConfig{DataDir: t.TempDir(), BaseDir: repoA}, env)
	t.Cleanup(func() { require.NoError(t, c.Close()) })

	stA, err := c.Register(context.Background(), RegisterRequest{Name: "default", Repo: repoA, PlanFile: planA})
	require.NoError(t, err)
	stB, err := c.Register(context.Background(), RegisterRequest{Name: "default", Repo: repoB, PlanFile: planB})
	require.NoError(t, err)

	scopeA := c.runs[stA.RunID].slotScope
	scopeB := c.runs[stB.RunID].slotScope
	require.Equal(t, "default", scopeA)
	require.NotEqual(t, scopeA, scopeB)
	require.Contains(t, scopeB, "_")
	require.Equal(t, ManagerSlotID(scopeA), "default-autopilot")
	require.Equal(t, ManagerSlotID(scopeB), scopeB+"-autopilot")
	require.NotEqual(t, ManagerSlotID(scopeA), ManagerSlotID(scopeB))
}

func TestPlanNameFooAutopilotCannotStealFooSlot(t *testing.T) {
	repo := t.TempDir()
	fooPlan := writePlan(t, repo, "foo.yaml", "foo")
	autopilotPlan := writePlan(t, repo, "foo-autopilot.yaml", "shadow")

	c := NewController(ControllerConfig{DataDir: t.TempDir(), BaseDir: repo}, &fakeEnv{})
	t.Cleanup(func() { require.NoError(t, c.Close()) })

	_, err := c.Register(context.Background(), RegisterRequest{Name: "foo", PlanFile: fooPlan})
	require.NoError(t, err)
	_, err = c.Register(context.Background(), RegisterRequest{Name: "foo-autopilot", PlanFile: autopilotPlan})
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrRunConflict))
	require.ErrorContains(t, err, "reserved suffix")
}

func TestRenameRunPreservesRunID(t *testing.T) {
	repo := t.TempDir()
	plan := writePlan(t, repo, "ship.yaml", "ship it")
	c := NewController(ControllerConfig{DataDir: t.TempDir(), BaseDir: repo}, &fakeEnv{})
	t.Cleanup(func() { require.NoError(t, c.Close()) })

	st, err := c.Register(context.Background(), RegisterRequest{Name: "ship", PlanFile: plan})
	require.NoError(t, err)
	oldScope := c.runs[st.RunID].slotScope

	renamed, err := c.RenameRun(context.Background(), st.RunID, "voyage")
	require.NoError(t, err)
	require.Equal(t, st.RunID, renamed.RunID)
	require.Equal(t, "voyage", renamed.Name)
	require.Equal(t, "voyage", c.runs[st.RunID].name)
	require.NotEqual(t, oldScope, c.runs[st.RunID].slotScope)
	require.Equal(t, ManagerSlotID("voyage"), ManagerSlotID(c.runs[st.RunID].slotScope))

	rec, err := c.store.Get(st.RunID)
	require.NoError(t, err)
	require.Equal(t, st.RunID, rec.RunID)
	require.Equal(t, "voyage", rec.Name)
	require.Equal(t, c.runs[st.RunID].slotScope, rec.SlotScope)
}

func TestEnableRejectsDuplicateRunNamesInRepo(t *testing.T) {
	repo := t.TempDir()
	p1 := writePlan(t, repo, "foo.yaml", "alpha")
	p2 := writePlan(t, repo, "foo.yml", "beta")
	c := NewController(ControllerConfig{Plans: []string{p1, p2}, BaseDir: repo, DataDir: t.TempDir()}, &fakeEnv{})
	t.Cleanup(func() { require.NoError(t, c.Close()) })

	_, err := c.Enable(context.Background(), repo)
	require.Error(t, err)
	var pfe *PreflightError
	require.ErrorAs(t, err, &pfe)
	require.NotEmpty(t, pfe.Failures)
}

func TestRegisterRejectsDuplicateNameInRepo(t *testing.T) {
	repo := t.TempDir()
	p1 := writePlan(t, repo, "one.yaml", "one")
	p2 := writePlan(t, repo, "two.yaml", "two")
	c := NewController(ControllerConfig{DataDir: t.TempDir(), BaseDir: repo}, &fakeEnv{})
	t.Cleanup(func() { require.NoError(t, c.Close()) })

	_, err := c.Register(context.Background(), RegisterRequest{Name: "dup", PlanFile: p1})
	require.NoError(t, err)
	_, err = c.Register(context.Background(), RegisterRequest{Name: "dup", PlanFile: p2})
	require.ErrorIs(t, err, ErrRunConflict)
}

func TestClaimRegistryRejectsScopeThatShadowsManagerSlot(t *testing.T) {
	cr := newClaimRegistry()
	require.NoError(t, cr.claim("ap-foo", "foo"))
	err := cr.validateClaim("ap-bar", "foo-autopilot")
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrRunConflict))
}
