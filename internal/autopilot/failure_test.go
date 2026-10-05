package autopilot

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// backoffAfterRotateFailure drives rotateStep on a brain-less run and returns the
// resulting backoff snapshot.
func backoffAfterRotateFailure(t *testing.T, mutate func(f *guardianFake, r *run)) *Backoff {
	t.Helper()
	t0 := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: t0}
	fake := newGuardianFake()
	c, runID := enabledGuardianController(t, fake, clock, cyclicResolver("a", "free"), testGuardian())
	r := c.runs[runID]
	r.brain = nil
	mutate(fake, r)
	c.rotateStep(context.Background(), fake, r, t0)
	b := c.Status().Runs[0].Backoff
	require.NotNil(t, b)
	return b
}

func TestSpawnFailureDefinitionErrorNotRateLimited(t *testing.T) {
	b := backoffAfterRotateFailure(t, func(_ *guardianFake, r *run) {
		require.NoError(t, os.Remove(r.absPlanFile))
		r.plan.Goal = ""
	})
	require.Equal(t, string(KindDefinitionError), b.Kind)
	require.Contains(t, b.LastError, "plan not loadable")
	require.NotContains(t, b.LastError, "rate-limited")
}

func TestSpawnFailureUnknownErrorNotRateLimited(t *testing.T) {
	b := backoffAfterRotateFailure(t, func(f *guardianFake, _ *run) {
		f.spawnErrOn["a"] = errors.New("tmux exploded")
	})
	require.Equal(t, string(KindSpawnError), b.Kind)
	require.Contains(t, b.LastError, "tmux exploded")
	require.NotContains(t, b.LastError, "rate-limited")
}

func TestSpawnFailureBackendUnavailable(t *testing.T) {
	b := backoffAfterRotateFailure(t, func(f *guardianFake, _ *run) {
		f.spawnErrOn["a"] = errors.Join(ErrBackendUnavailable, errors.New("claude: 429"))
	})
	require.Equal(t, string(KindBackendUnavailable), b.Kind)
	require.Contains(t, b.LastError, "429")
	require.NotContains(t, b.LastError, "all backends rate-limited")
}

func TestSpawnFailureNoBackendSelectable(t *testing.T) {
	b := backoffAfterRotateFailure(t, func(_ *guardianFake, r *run) {
		r.tried["a"] = true // the only backend is already tried ⇒ nothing selectable
	})
	require.Equal(t, string(KindNoBackendSelectable), b.Kind)
	require.Contains(t, b.LastError, "all backends rate-limited")
}

func TestClassifySpawnError(t *testing.T) {
	require.Equal(t, KindDefinitionError, classifySpawnError(tagFailure(KindDefinitionError, errors.New("x"))))
	require.Equal(t, KindBackendUnavailable, classifySpawnError(errors.New("hit usage limit")))
	require.Equal(t, KindSpawnError, classifySpawnError(errors.New("boom")))
}
