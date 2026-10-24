package autopilot

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type migrationFake struct {
	*fakeRuntime
	calls      int
	mu         sync.Mutex
	reconciled []BootReconcileRun
}

func (m *migrationFake) ReconcileSessions(_ context.Context, runs []BootReconcileRun) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
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
		State: StateRegistered, BrainID: "agent-deadbeef",
		CreatedAt: now, UpdatedAt: now,
	}))
	require.NoError(t, runStore.Close())

	c := NewController(ControllerConfig{DataDir: data, BaseDir: repo, IntegrationBranch: DefaultIntegrationBranch}, &fakeEnv{})
	mf := &migrationFake{fakeRuntime: newFakeRuntime()}
	setRuntimeAndWait(t, c, mf)
	t.Cleanup(func() { require.NoError(t, c.Close()) })

	require.Eventually(t, func() bool {
		c.mu.Lock()
		branch := c.runs[id].integrationBranch
		c.mu.Unlock()
		mf.mu.Lock()
		defer mf.mu.Unlock()
		return branch == DefaultIntegrationBranch && len(mf.reconciled) == 1
	}, time.Second, 10*time.Millisecond)
	require.Equal(t, "agent-deadbeef", mf.reconciled[0].LegacyBrainID)
	require.Equal(t, ManagerSlotID("legacy"), mf.reconciled[0].ManagerSlotID)
}

func TestReconcileRunsAtBootIdempotent(t *testing.T) {
	repo := t.TempDir()
	plan := writePlan(t, repo, "ship.yaml", "ship")
	data := t.TempDir()
	env := &fakeEnv{repoOf: func(string) (string, error) { return repo, nil }}

	c := NewController(ControllerConfig{DataDir: data, BaseDir: repo, Resolver: &fakeResolver{backendID: "a", tier: "free"}}, env)
	mf := &migrationFake{fakeRuntime: newFakeRuntime()}
	setRuntimeAndWait(t, c, mf)
	_, err := c.Register(context.Background(), RegisterRequest{Name: "ship", PlanFile: plan})
	require.NoError(t, err)

	require.Eventually(t, func() bool { mf.mu.Lock(); defer mf.mu.Unlock(); return mf.calls == 1 }, time.Second, time.Millisecond)
	mf.mu.Lock()
	defer mf.mu.Unlock()
	c.mu.Lock()
	c.reconcileRunsAtBootLocked(context.Background())
	first := mf.calls
	c.reconcileRunsAtBootLocked(context.Background())
	c.mu.Unlock()
	require.Equal(t, first, mf.calls)
	require.Equal(t, 1, first)
}
