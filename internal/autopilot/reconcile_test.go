package autopilot

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type migrationFake struct {
	*guardianAgentFake
	reconciled []BootReconcileRun
}

func (m *migrationFake) ReconcileSessions(_ context.Context, runs []BootReconcileRun) error {
	m.reconciled = append([]BootReconcileRun(nil), runs...)
	return nil
}

func TestReconcileRunsAtBootGrandfathersEmptyIntegrationBranch(t *testing.T) {
	repo := t.TempDir()
	plan := writePlan(t, repo, "legacy.yaml", "legacy")
	data := t.TempDir()
	runStore, err := NewRunStore(data)
	require.NoError(t, err)
	t.Cleanup(func() { _ = runStore.Close() })

	id := RunID(repo, plan)
	now := time.Now().UTC()
	require.NoError(t, runStore.Create(RunRecord{
		RunID: id, Name: "legacy", Repo: repo, PlanFile: plan,
		State: StateRegistered, BrainID: "agent-deadbeef", GuardianID: "guardian-deadbeef",
		CreatedAt: now, UpdatedAt: now,
	}))
	require.NoError(t, runStore.Close())

	c := NewController(ControllerConfig{DataDir: data, BaseDir: repo, IntegrationBranch: DefaultIntegrationBranch}, &fakeEnv{})
	mf := &migrationFake{guardianAgentFake: &guardianAgentFake{fakeRuntime: newFakeRuntime()}}
	c.SetRuntime(mf)
	t.Cleanup(func() { require.NoError(t, c.Close()) })

	require.Equal(t, DefaultIntegrationBranch, c.runs[id].integrationBranch)
	require.Len(t, mf.reconciled, 1)
	require.Equal(t, "agent-deadbeef", mf.reconciled[0].LegacyBrainID)
	require.Equal(t, ManagerSlotID("legacy"), mf.reconciled[0].ManagerSlotID)
}

func TestSetRuntimeReconcileGuardiansUsesSlotIDs(t *testing.T) {
	dir := t.TempDir()
	plan := writePlan(t, dir, "plan.yaml", "ship")
	rt := &migrationFake{guardianAgentFake: &guardianAgentFake{fakeRuntime: newFakeRuntime()}}
	c := NewController(ControllerConfig{
		Plans: []string{plan}, BaseDir: dir, Resolver: &fakeResolver{backendID: "a", tier: "free"},
	}, &fakeEnv{})
	c.SetRuntime(rt)
	st, err := c.Enable(context.Background(), "")
	require.NoError(t, err)
	runID := st.Runs[0].RunID
	c.SetRuntime(rt) // daemon re-attach after runs are live
	require.Equal(t, map[string]string{runID: GuardianSlotID("plan")}, rt.guardianAgentFake.reconciled)
}

func TestReconcileRunsAtBootIdempotent(t *testing.T) {
	repo := t.TempDir()
	plan := writePlan(t, repo, "ship.yaml", "ship")
	data := t.TempDir()
	env := &fakeEnv{repoOf: func(string) (string, error) { return repo, nil }}

	c := NewController(ControllerConfig{DataDir: data, BaseDir: repo, Resolver: &fakeResolver{backendID: "a", tier: "free"}}, env)
	mf := &migrationFake{guardianAgentFake: &guardianAgentFake{fakeRuntime: newFakeRuntime()}}
	c.SetRuntime(mf)
	_, err := c.Register(context.Background(), RegisterRequest{Name: "ship", PlanFile: plan})
	require.NoError(t, err)

	c.mu.Lock()
	c.reconcileRunsAtBootLocked(context.Background())
	first := len(mf.reconciled)
	c.reconcileRunsAtBootLocked(context.Background())
	c.mu.Unlock()
	require.Equal(t, first, len(mf.reconciled))
	require.Equal(t, 1, first)
}
