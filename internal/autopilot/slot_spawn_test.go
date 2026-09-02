package autopilot

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSlotSpawnFirstStartCreatesSlotIds verifies the first start persists stable
// manager and guardian slot ids on the run record (WP4 acceptance).
func TestSlotSpawnFirstStartCreatesSlotIds(t *testing.T) {
	repo := t.TempDir()
	plan := writePlan(t, repo, "ship.yaml", "ship it")
	data := t.TempDir()
	env := &fakeEnv{repoOf: func(string) (string, error) { return repo, nil }}
	runStore, err := NewRunStore(data)
	require.NoError(t, err)
	t.Cleanup(func() { _ = runStore.Close() })

	rt := &guardianAgentFake{fakeRuntime: newFakeRuntime()}
	c := NewController(ControllerConfig{
		DataDir:           data,
		BaseDir:           repo,
		IntegrationBranch: "autopilot/integration",
		Resolver:          &fakeResolver{backendID: "a", tier: "free"},
		RunStore:          runStore,
	}, env)
	c.SetRuntime(rt)

	r, err := c.Register(context.Background(), RegisterRequest{Name: "ship", PlanFile: plan})
	require.NoError(t, err)
	scope := c.runs[r.RunID].slotScope
	require.NotEmpty(t, scope)

	_, err = c.StartRun(context.Background(), r.RunID)
	require.NoError(t, err)

	managerID := ManagerSlotID(scope)
	guardianID := GuardianSlotID(scope)
	require.Equal(t, managerID, c.Status().Runs[0].Brain.AgentID)
	require.Equal(t, guardianID, c.Status().Runs[0].GuardianID)
	require.Len(t, rt.spawned, 1)
	require.Equal(t, scope, rt.spawned[0].SlotScope)

	rec, err := runStore.Get(r.RunID)
	require.NoError(t, err)
	require.Equal(t, managerID, rec.BrainID)
	require.Equal(t, guardianID, rec.GuardianID)
}

// TestSlotSpawnSecondStartAdopts verifies a second controller boot adopts the
// same slot ids without minting new manager ids (WP4 acceptance).
func TestSlotSpawnSecondStartAdopts(t *testing.T) {
	repo := t.TempDir()
	plan := writePlan(t, repo, "ship.yaml", "ship it")
	data := t.TempDir()
	env := &fakeEnv{repoOf: func(string) (string, error) { return repo, nil }}

	c1 := NewController(ControllerConfig{
		DataDir:           data,
		BaseDir:           repo,
		IntegrationBranch: "autopilot/integration",
		Resolver:          &fakeResolver{backendID: "a", tier: "free"},
	}, env)
	rt1 := newFakeRuntime()
	c1.SetRuntime(rt1)
	r, err := c1.Register(context.Background(), RegisterRequest{Name: "ship", PlanFile: plan})
	require.NoError(t, err)
	_, err = c1.StartRun(context.Background(), r.RunID)
	require.NoError(t, err)
	firstBrain := c1.Status().Runs[0].Brain.AgentID
	require.NoError(t, c1.Close())

	c2 := NewController(ControllerConfig{
		DataDir:           data,
		BaseDir:           repo,
		IntegrationBranch: "autopilot/integration",
		Resolver:          &fakeResolver{backendID: "a", tier: "free"},
	}, env)
	rt2 := newFakeRuntime()
	c2.SetRuntime(rt2)
	t.Cleanup(func() { require.NoError(t, c2.Close()) })

	require.Equal(t, firstBrain, c2.Status().Runs[0].Brain.AgentID)
	require.Len(t, rt2.spawned, 1)
}
