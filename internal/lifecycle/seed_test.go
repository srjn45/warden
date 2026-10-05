package lifecycle

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// fastSeed shrinks the seed timing knobs and restores them afterwards.
func fastSeed(t *testing.T) {
	t.Helper()
	oa, ob, oi, os_, ot := promptSeedAttempts, promptSeedRetryBackoff, inputSubmitDelay, promptSeedSettle, promptSeedPollInterval
	promptSeedAttempts, promptSeedRetryBackoff, inputSubmitDelay, promptSeedSettle, promptSeedPollInterval = 3, time.Millisecond, 0, 0, time.Millisecond
	t.Cleanup(func() {
		promptSeedAttempts, promptSeedRetryBackoff, inputSubmitDelay, promptSeedSettle, promptSeedPollInterval = oa, ob, oi, os_, ot
	})
}

func crushBackend(t *testing.T) agentbackend.Backend {
	b, err := agentbackend.Get("crush")
	require.NoError(t, err)
	return b
}

// seedRunner reports the crush ready marker and fails the first failPastes
// paste-buffer calls.
func seedRunner(failPastes int) *FakeRunner {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"tmux capture-pane -p -t crush-1": {Out: "ctrl+p commands"},
	}}
	n := 0
	fr.FailIf = func(argv []string) error {
		if len(argv) > 1 && argv[1] == "paste-buffer" {
			n++
			if n <= failPastes {
				return errors.New("no server running")
			}
		}
		return nil
	}
	return fr
}

func pastes(fr *FakeRunner) int {
	n := 0
	for _, a := range fr.calledArgs() {
		if len(a) > 1 && a[1] == "paste-buffer" {
			n++
		}
	}
	return n
}

func TestSeedRetriesThenSucceeds(t *testing.T) {
	fastSeed(t)
	fr := seedRunner(2)
	lc := New(fr, &FakeConfig{})
	lc.PromptsDir = t.TempDir()
	got := make(chan SeedOutcome, 1)
	lc.OnSeed = func(o SeedOutcome) { got <- o }

	agent := &agentstore.Agent{ID: "crush-1"}
	lc.seedInteractivePrompt(crushBackend(t), agent, "do the thing")
	require.Equal(t, store.SeedPending, agent.SeedStatus, "spawn persists the pending stamp")

	o := <-got
	require.Equal(t, store.SeedDelivered, o.Status)
	require.Empty(t, o.Err)
	require.Equal(t, 3, pastes(fr), "two failed pastes then one that lands")
}

func TestSeedFinalFailureReportsOnce(t *testing.T) {
	fastSeed(t)
	fr := seedRunner(100)
	lc := New(fr, &FakeConfig{})
	lc.PromptsDir = t.TempDir()
	got := make(chan SeedOutcome, 4)
	lc.OnSeed = func(o SeedOutcome) { got <- o }

	lc.seedInteractivePrompt(crushBackend(t), &agentstore.Agent{ID: "crush-1"}, "do the thing")
	o := <-got
	require.Equal(t, store.SeedFailed, o.Status)
	require.Contains(t, o.Err, "paste failed after 3 attempts")
	require.Equal(t, filepath.Join(lc.PromptsDir, "crush-1"), o.PromptFile)
	require.Equal(t, 3, pastes(fr))
	select {
	case extra := <-got:
		t.Fatalf("outcome reported twice: %+v", extra)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestSeedUINeverReadyFails(t *testing.T) {
	fastSeed(t)
	old := promptSeedTimeout
	promptSeedTimeout = 30 * time.Millisecond
	t.Cleanup(func() { promptSeedTimeout = old })
	lc := New(&FakeRunner{}, &FakeConfig{}) // pane never shows the marker
	got := make(chan SeedOutcome, 1)
	lc.OnSeed = func(o SeedOutcome) { got <- o }
	lc.seedInteractivePrompt(crushBackend(t), &agentstore.Agent{ID: "crush-1"}, "x")
	o := <-got
	require.Equal(t, store.SeedFailed, o.Status)
	require.Contains(t, o.Err, "not ready")
}

// The prompt file is written for a typed-prompt backend on every path that seeds
// (all five call sites funnel through seedInteractivePrompt), and a launch-line
// backend is untouched.
func TestSeedWritesPromptFileAndSkipsLaunchLineBackends(t *testing.T) {
	fastSeed(t)
	lc := New(seedRunner(0), &FakeConfig{})
	lc.PromptsDir = t.TempDir()
	got := make(chan SeedOutcome, 2)
	lc.OnSeed = func(o SeedOutcome) { got <- o }
	lc.run = &realFSRunner{FakeRunner: seedRunner(0)}

	claude, err := agentbackend.Get("claude")
	require.NoError(t, err)
	a := &agentstore.Agent{ID: "claude-1"}
	lc.seedInteractivePrompt(claude, a, "launch-line prompt")
	require.Empty(t, a.SeedStatus, "launch-line backends carry no seed status")
	_, err = os.Stat(filepath.Join(lc.PromptsDir, "claude-1"))
	require.True(t, os.IsNotExist(err))

	b := &agentstore.Agent{ID: "crush-1"}
	lc.seedInteractivePrompt(crushBackend(t), b, "multi\nline prompt")
	require.Equal(t, store.SeedDelivered, (<-got).Status) // wait out the goroutine
	data, err := os.ReadFile(filepath.Join(lc.PromptsDir, "crush-1"))
	require.NoError(t, err)
	require.Equal(t, "multi\nline prompt", string(data))
	st, _ := os.Stat(filepath.Join(lc.PromptsDir, "crush-1"))
	require.Equal(t, os.FileMode(0o600), st.Mode().Perm())
}

// realFSRunner executes the prompt-file write (sh/mkdir) for real and fakes tmux.
type realFSRunner struct{ *FakeRunner }

func (r *realFSRunner) Run(ctx_ context.Context, dir, name string, args ...string) (string, error) {
	if name == "tmux" {
		return r.FakeRunner.Run(ctx_, dir, name, args...)
	}
	return ExecRunner{}.Run(ctx_, dir, name, args...)
}
