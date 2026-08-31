package autopilot

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUpdateTaskStatusValidationConcurrencyAndRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.yaml")
	require.NoError(t, os.WriteFile(path, []byte("goal: g\ntasks:\n  - id: a\n    prompt: a\n  - id: b\n    prompt: b\n"), 0o644))
	rt := newFakeRuntime()
	c := NewController(ControllerConfig{Plans: []string{path}, BaseDir: dir}, &fakeEnv{})
	c.SetRuntime(rt)
	st, err := c.Enable(context.Background(), dir)
	require.NoError(t, err)
	runID := st.Runs[0].RunID

	_, err = c.UpdateTaskStatus(runID, "a", TaskStatusDone, 7)
	require.ErrorContains(t, err, "not recorded as landed")
	require.NoError(t, rt.NewLedger(runID).AppendLanding(Landing{Branch: "x", SHA: "s", PR: 7, LandedAt: "now"}))

	var wg sync.WaitGroup
	for i, id := range []string{"a", "b"} {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			_, e := c.UpdateTaskStatus(runID, id, TaskStatusActive, 0)
			require.NoError(t, e, fmt.Sprint(i))
		}(i, id)
	}
	wg.Wait()
	_, err = c.UpdateTaskStatus(runID, "a", TaskStatusDone, 7)
	require.NoError(t, err)

	// A fresh controller reconstructs progress solely from the plan task ledger.
	restarted := NewController(ControllerConfig{Plans: []string{path}, BaseDir: dir}, &fakeEnv{})
	restarted.SetRuntime(rt)
	_, err = restarted.Enable(context.Background(), dir)
	require.NoError(t, err)
	restarted.mu.Lock()
	got := restarted.runs[runID].plan.Tasks
	restarted.mu.Unlock()
	require.Equal(t, TaskStatusDone, got[0].Status)
	require.Equal(t, 7, got[0].LandedPR)
	require.Equal(t, TaskStatusActive, got[1].Status)
}
