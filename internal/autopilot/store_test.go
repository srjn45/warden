package autopilot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type failingEnableStore struct{ err error }

func (s failingEnableStore) Enable(string) error { return s.err }
func (failingEnableStore) Disable(string) error  { return nil }
func (failingEnableStore) IsEnabled(string) bool { return false }
func (failingEnableStore) List() []string        { return nil }

func TestRunStoreReopenAndConcurrentRMW(t *testing.T) {
	dir := t.TempDir()
	s, err := NewRunStore(dir)
	require.NoError(t, err)
	now := time.Now().UTC()
	require.NoError(t, s.Create(RunRecord{RunID: "ap-one", Name: "one", State: StateRegistered, CreatedAt: now, UpdatedAt: now}))
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, e := s.Update("ap-one", func(r *RunRecord) error { r.Name += "x"; return nil })
			require.NoError(t, e)
		}()
	}
	wg.Wait()
	rec, err := s.Get("ap-one")
	require.NoError(t, err)
	require.Len(t, rec.Name, 19)
	require.NoError(t, s.Close())
	s, err = NewRunStore(dir)
	require.NoError(t, err)
	defer s.Close()
	rec, err = s.Get("ap-one")
	require.NoError(t, err)
	require.Equal(t, StateRegistered, rec.State)
}

func TestConfiguredRunStoreFailureFailsClosed(t *testing.T) {
	badDataDir := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(t, os.WriteFile(badDataDir, []byte("x"), 0o600))
	c := NewController(ControllerConfig{DataDir: badDataDir}, &fakeEnv{})
	_, err := c.Register(context.Background(), RegisterRequest{PlanFile: "plan.yaml"})
	require.ErrorContains(t, err, "persistent run store unavailable")
}

func TestStartRunRetriesAfterEnableStoreWriteFailure(t *testing.T) {
	repo := t.TempDir()
	plan := writePlan(t, repo, "retry.yaml", "retry")
	c := NewController(ControllerConfig{BaseDir: repo}, &fakeEnv{})
	r, err := c.Register(context.Background(), RegisterRequest{PlanFile: plan})
	require.NoError(t, err)
	c.enableStore = failingEnableStore{err: errors.New("disk full")}
	_, err = c.StartRun(context.Background(), r.RunID)
	require.ErrorContains(t, err, "disk full")
	require.Equal(t, StateRegistered, c.runs[r.RunID].state)

	c.enableStore = newMemEnableStore()
	got, err := c.StartRun(context.Background(), r.RunID)
	require.NoError(t, err)
	require.Equal(t, StateActive, got.State)
}

func TestMultiRunLifecyclePersists(t *testing.T) {
	dir := t.TempDir()
	p1 := writePlan(t, dir, "one.yaml", "one")
	p2 := writePlan(t, dir, "two.yaml", "two")
	s, err := NewRunStore(dir)
	require.NoError(t, err)
	c := NewController(ControllerConfig{BaseDir: dir, RunStore: s}, &fakeEnv{})
	r1, err := c.Register(context.Background(), RegisterRequest{Name: "one", PlanFile: p1})
	require.NoError(t, err)
	r2, err := c.Register(context.Background(), RegisterRequest{Name: "two", PlanFile: p2})
	require.NoError(t, err)
	require.NotEqual(t, r1.RunID, r2.RunID)
	_, err = c.StartRun(context.Background(), r1.RunID)
	require.NoError(t, err)
	land, ok := c.LandParams(r1.RunID)
	require.True(t, ok)
	require.Equal(t, "main", land.DefaultBranch, "registered start must run merge-safety preflight")
	require.Equal(t, "local", land.Gate)
	_, err = c.StartRun(context.Background(), r2.RunID)
	require.NoError(t, err)
	_, err = c.PauseRun(context.Background(), r1.RunID)
	require.NoError(t, err)
	_, err = c.ResumeRun(context.Background(), r1.RunID)
	require.NoError(t, err)
	_, err = c.StopRun(context.Background(), r2.RunID)
	require.NoError(t, err)
	require.NoError(t, c.Close())

	s2, err := NewRunStore(dir)
	require.NoError(t, err)
	c2 := NewController(ControllerConfig{BaseDir: dir, RunStore: s2}, &fakeEnv{})
	defer c2.Close()
	states := map[string]RunState{}
	for _, r := range c2.Status().Runs {
		states[r.Name] = r.State
	}
	require.Equal(t, StateActive, states["one"])
	require.Equal(t, StateStopped, states["two"])
	require.Equal(t, RunID(dir, p1), r1.RunID, fmt.Sprint(states))
}

func TestUnregisterRunRemovesDurableRegistrationAndRejectsLiveRun(t *testing.T) {
	dir := t.TempDir()
	p1 := writePlan(t, dir, "one.yaml", "one")
	p2 := writePlan(t, dir, "two.yaml", "two")
	s, err := NewRunStore(dir)
	require.NoError(t, err)
	c := NewController(ControllerConfig{BaseDir: dir, RunStore: s}, &fakeEnv{})
	r1, err := c.Register(context.Background(), RegisterRequest{Name: "one", PlanFile: p1})
	require.NoError(t, err)
	r2, err := c.Register(context.Background(), RegisterRequest{Name: "two", PlanFile: p2})
	require.NoError(t, err)
	_, err = c.StartRun(context.Background(), r2.RunID)
	require.NoError(t, err)

	removed, err := c.UnregisterRun(context.Background(), r1.RunID)
	require.NoError(t, err)
	require.Equal(t, r1.RunID, removed.RunID)
	_, err = c.UnregisterRun(context.Background(), r2.RunID)
	require.ErrorIs(t, err, ErrRunConflict)
	require.NoError(t, c.Close())

	s2, err := NewRunStore(dir)
	require.NoError(t, err)
	c2 := NewController(ControllerConfig{BaseDir: dir, RunStore: s2}, &fakeEnv{})
	defer c2.Close()
	require.NotContains(t, mapRunStates(c2.Status().Runs), "one")
	require.Contains(t, mapRunStates(c2.Status().Runs), "two")
}

func mapRunStates(runs []RunStatus) map[string]RunState {
	out := make(map[string]RunState, len(runs))
	for _, r := range runs {
		out[r.Name] = r.State
	}
	return out
}

func TestEnableAllowsTwoPlansInSameRepo(t *testing.T) {
	dir := t.TempDir()
	p1 := writePlan(t, dir, "a.yaml", "a")
	p2 := writePlan(t, dir, "b.yaml", "b")
	c := NewController(ControllerConfig{Plans: []string{p1, p2}, BaseDir: dir}, &fakeEnv{})
	st, err := c.Enable(context.Background(), dir)
	require.NoError(t, err)
	require.Len(t, st.Runs, 2)
}

func TestRegisteredRunSurvivesConfigReloadWithoutLegacyPlans(t *testing.T) {
	repo := t.TempDir()
	plan := filepath.Join(repo, "plans", "named.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(plan), 0o755))
	require.NoError(t, os.WriteFile(plan, []byte("version: 1\ngoal: durable\n"), 0o644))
	env := &fakeEnv{repoOf: func(string) (string, error) { return repo, nil }}
	c := NewController(ControllerConfig{DataDir: t.TempDir(), BaseDir: repo}, env)
	t.Cleanup(func() { require.NoError(t, c.Close()) })
	r, err := c.Register(context.Background(), RegisterRequest{Name: "named", PlanFile: plan})
	require.NoError(t, err)

	c.Reconfigure(context.Background(), ControllerConfig{Plans: nil, BaseDir: repo})
	got := c.Status()
	require.Len(t, got.Runs, 1)
	require.Equal(t, r.RunID, got.Runs[0].RunID)
	require.Equal(t, StateRegistered, got.Runs[0].State)
}

func TestRegisteredActiveRunRecoversWithoutLegacyConfig(t *testing.T) {
	repo := t.TempDir()
	plan := writePlan(t, repo, "named.yaml", "durable")
	data := t.TempDir()
	env := &fakeEnv{repoOf: func(string) (string, error) { return repo, nil }}

	c1 := NewController(ControllerConfig{DataDir: data, BaseDir: repo}, env)
	r, err := c1.Register(context.Background(), RegisterRequest{Name: "named", PlanFile: plan})
	require.NoError(t, err)
	rt1 := newFakeRuntime()
	c1.SetRuntime(rt1)
	_, err = c1.StartRun(context.Background(), r.RunID)
	require.NoError(t, err)
	require.NoError(t, c1.Close())

	c2 := NewController(ControllerConfig{DataDir: data, BaseDir: repo}, env)
	t.Cleanup(func() { require.NoError(t, c2.Close()) })
	rt2 := newFakeRuntime()
	c2.SetRuntime(rt2)
	require.Len(t, rt2.spawned, 1, "durable active intent must recover without autopilot.plans")
	require.Equal(t, r.RunID, rt2.spawned[0].RunID)
	require.Equal(t, StateActive, c2.Status().Runs[0].State)
}

func TestPausedRunResumeAfterRestartSpawnsBrain(t *testing.T) {
	repo := t.TempDir()
	plan := writePlan(t, repo, "paused.yaml", "durable")
	data := t.TempDir()
	env := &fakeEnv{repoOf: func(string) (string, error) { return repo, nil }}
	c1 := NewController(ControllerConfig{DataDir: data, BaseDir: repo}, env)
	r, err := c1.Register(context.Background(), RegisterRequest{Name: "paused", PlanFile: plan})
	require.NoError(t, err)
	c1.SetRuntime(newFakeRuntime())
	_, err = c1.StartRun(context.Background(), r.RunID)
	require.NoError(t, err)
	_, err = c1.PauseRun(context.Background(), r.RunID)
	require.NoError(t, err)
	require.NoError(t, c1.Close())

	c2 := NewController(ControllerConfig{DataDir: data, BaseDir: repo}, env)
	t.Cleanup(func() { require.NoError(t, c2.Close()) })
	rt2 := newFakeRuntime()
	c2.SetRuntime(rt2)
	require.Empty(t, rt2.spawned, "paused intent must not start work at boot")
	got, err := c2.ResumeRun(context.Background(), r.RunID)
	require.NoError(t, err)
	require.Len(t, rt2.spawned, 1)
	require.Equal(t, StateActive, got.State)
}
