package autopilot

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/planstore"
	"github.com/stretchr/testify/require"
)

// parkHarness builds a brain-less run whose spawn can be scripted to fail, bound
// to a plan in a fake plan source so plan-change exits can be exercised.
type parkHarness struct {
	t     *testing.T
	c     *Controller
	r     *run
	fake  *guardianFake
	clock *fakeClock
	src   *fakePlanSource
}

func newParkHarness(t *testing.T) *parkHarness {
	t.Helper()
	t0 := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: t0}
	fake := newGuardianFake()
	c, runID := enabledGuardianController(t, fake, clock, cyclicResolver("a", "free"), testGuardian())
	r := c.runs[runID]
	r.brain = nil
	r.planID = "plan-park"
	src := &fakePlanSource{plans: map[string]*planstore.Plan{"plan-park": {ID: "plan-park", Revision: 1, ContentHash: "sha256:one"}}}
	c.planSource = src
	return &parkHarness{t: t, c: c, r: r, fake: fake, clock: clock, src: src}
}

// tick advances past any backoff and runs one guardian pass.
func (h *parkHarness) tick() {
	h.clock.t = h.clock.t.Add(7 * time.Hour)
	h.c.guardianTick(context.Background())
}

func (h *parkHarness) attention() string { return h.c.Status().Runs[0].NeedsAttention }

func countAudit(f *guardianFake, prefix string) int {
	n := 0
	for _, a := range f.audits {
		if len(a) >= len(prefix) && a[:len(prefix)] == prefix {
			n++
		}
	}
	return n
}

func TestDefinitionErrorParksAfterOneAttemptAndNotifiesOnce(t *testing.T) {
	h := newParkHarness(t)
	h.fake.spawnErrOn["a"] = tagFailure(KindDefinitionError, errors.New("plan not loadable"))
	h.tick()
	require.Contains(t, h.attention(), "plan not loadable")
	require.Equal(t, StateDegraded, h.c.Status().Runs[0].State)
	require.Len(t, h.fake.escalations, 1)
	require.Equal(t, 1, countAudit(h.fake, "autopilot.needs_attention"))
	spawns := len(h.fake.spawned) + len(h.fake.rotated)
	for i := 0; i < 5; i++ {
		h.tick()
	}
	require.Len(t, h.fake.escalations, 1, "no further notifications while parked")
	require.Equal(t, 1, countAudit(h.fake, "autopilot.needs_attention"))
	require.Equal(t, spawns, len(h.fake.spawned)+len(h.fake.rotated), "no further spawn attempts")
}

func TestRepeatedUnknownErrorParksAtThreshold(t *testing.T) {
	h := newParkHarness(t)
	h.c.guardian.MaxIdenticalFailures = 3
	h.fake.spawnErrOn["a"] = errors.New("tmux exploded")
	for i := 0; i < 2; i++ {
		h.tick()
		require.Empty(t, h.attention(), "attempt %d must still back off", i+1)
	}
	h.tick()
	require.Contains(t, h.attention(), "tmux exploded")
	require.Contains(t, h.attention(), string(KindSpawnError))
}

func TestChangingUnknownErrorResetsStreak(t *testing.T) {
	h := newParkHarness(t)
	h.c.guardian.MaxIdenticalFailures = 2
	h.fake.spawnErrOn["a"] = errors.New("boom 1")
	h.tick()
	h.fake.spawnErrOn["a"] = errors.New("boom 2")
	h.tick()
	require.Empty(t, h.attention())
}

func TestTransientErrorsNeverPark(t *testing.T) {
	h := newParkHarness(t)
	h.fake.spawnErrOn["a"] = errors.Join(ErrBackendUnavailable, errors.New("429"))
	for i := 0; i < 12; i++ {
		h.tick()
	}
	require.Empty(t, h.attention())
	require.NotNil(t, h.c.Status().Runs[0].Backoff)
}

func parkRun(t *testing.T) *parkHarness {
	h := newParkHarness(t)
	h.fake.spawnErrOn["a"] = tagFailure(KindDefinitionError, errors.New("plan not loadable"))
	h.tick()
	require.NotEmpty(t, h.attention())
	h.fake.spawnErrOn["a"] = nil
	delete(h.fake.spawnErrOn, "a")
	return h
}

func TestParkExitsOnPlanChange(t *testing.T) {
	h := parkRun(t)
	h.tick()
	require.NotEmpty(t, h.attention(), "unchanged plan stays parked")
	h.src.plans["plan-park"] = &planstore.Plan{ID: "plan-park", Revision: 2, ContentHash: "sha256:two"}
	h.tick()
	require.Empty(t, h.attention())
	require.NotNil(t, h.c.runs[h.r.runID].brain, "healing retried and respawned the manager")
}

func TestParkExitsOnPauseResume(t *testing.T) {
	h := parkRun(t)
	_, err := h.c.PauseRun(context.Background(), h.r.runID)
	require.NoError(t, err)
	require.Empty(t, h.attention())
	// Re-park, then resume directly from the paused state.
	h.r.needsAttention = "x"
	h.r.state = StatePaused
	_, _ = h.c.ResumeRun(context.Background(), h.r.runID)
	require.Empty(t, h.attention())
}

func TestMaxIdenticalFailuresDefault(t *testing.T) {
	require.Equal(t, 5, withGuardianDefaults(GuardianParams{}).MaxIdenticalFailures)
}
