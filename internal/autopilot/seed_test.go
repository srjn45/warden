package autopilot

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// seedFake adds SeedRuntime to guardianFake: a scriptable seed status and a
// redelivery sink.
type seedFake struct {
	*guardianFake
	status      map[string]string
	redelivered []string
	redeliverEr error
}

func (f *seedFake) SeedState(_ context.Context, id string) (string, bool) {
	s, ok := f.status[id]
	return s, ok
}

func (f *seedFake) RedeliverSeed(_ context.Context, id, text string) error {
	if f.redeliverEr != nil {
		return f.redeliverEr
	}
	f.redelivered = append(f.redelivered, id+": "+text)
	f.status[id] = store.SeedDelivered
	return nil
}

func newSeedController(t *testing.T) (*Controller, *seedFake, *fakeClock, string) {
	t0 := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: t0}
	fake := &seedFake{guardianFake: newGuardianFake(), status: map[string]string{}}
	c, runID := enabledGuardianController(t, fake.guardianFake, clock, cyclicResolver("a", "free"), testGuardian())
	c.SetRuntime(fake) // swap in the SeedRuntime-capable wrapper
	return c, fake, clock, runID
}

func TestGuardianRedeliversDigestOnFailedSeed(t *testing.T) {
	c, fake, clock, runID := newSeedController(t)
	ctx := context.Background()
	id := c.runs[runID].brain.AgentID
	fake.status[id] = store.SeedFailed
	fake.activity[runID] = clock.t // heartbeat is fresh: only the seed is wrong

	c.guardianTick(ctx)
	require.Len(t, fake.redelivered, 1, "digest re-delivered exactly once")
	require.Contains(t, fake.redelivered[0], "Autopilot run digest")
	require.Equal(t, StateActive, c.runs[runID].state, "a successful re-delivery leaves the run active")
	require.Contains(t, fake.audits, "autopilot.seed_redelivered:"+id)

	c.guardianTick(ctx)
	require.Len(t, fake.redelivered, 1, "delivered now: no second re-delivery")
}

func TestGuardianEscalatesWhenRedeliveryFails(t *testing.T) {
	c, fake, clock, runID := newSeedController(t)
	ctx := context.Background()
	id := c.runs[runID].brain.AgentID
	fake.status[id] = store.SeedFailed
	fake.redeliverEr = errors.New("tmux paste-buffer: no server running")
	fake.activity[runID] = clock.t

	c.guardianTick(ctx)
	r := c.runs[runID]
	require.Equal(t, StateDegraded, r.state, "run must not stay active with a promptless manager")
	require.Equal(t, stageBackoff, r.healStage, "escalated onto the existing heal ladder")
	require.Contains(t, r.backoffLastErr, "never received its prompt")
	require.NotEmpty(t, fake.escalations, "operator notified through the existing escalation path")

	n := len(fake.escalations)
	c.guardianTick(ctx)
	require.Len(t, fake.escalations, n, "escalated once, not every tick")
}

func TestOverwatchTellsManagerWorkerSeedFailed(t *testing.T) {
	c, fake, clock, runID := newSeedController(t)
	ctx := context.Background()
	mgr := c.runs[runID].brain.AgentID
	clock.t = clock.t.Add(time.Minute)
	fake.roster[runID] = []AgentInfo{
		{ID: mgr, State: "working"},
		{ID: "w-1", Name: "worker-one", State: "idle", SeedStatus: store.SeedFailed},
	}
	// A busy manager is left alone; once idle it is told exactly once.
	c.overwatchTick(ctx)
	require.Empty(t, fake.wakes)
	fake.roster[runID][0].State = "idle"
	c.overwatchTick(ctx)
	require.Len(t, fake.wakes, 1)
	require.Contains(t, fake.wakes[0], "never received their initial prompt")
	require.Contains(t, fake.wakes[0], "w-1")
	c.overwatchTick(ctx)
	for _, w := range fake.wakes[1:] {
		require.NotContains(t, w, "never received their initial prompt", "told once per worker")
	}
}
