package autopilot

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

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
