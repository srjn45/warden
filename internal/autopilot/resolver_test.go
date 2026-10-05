package autopilot

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// resolverFake adds the ResolverRuntime seam to the guardian fake.
type resolverFake struct {
	*guardianFake
	spawned []ResolverSpawn
}

func (f *resolverFake) SpawnResolver(_ context.Context, s ResolverSpawn) (string, error) {
	f.spawned = append(f.spawned, s)
	return "resolver-1", nil
}

func resolverSetup(t *testing.T) (*Controller, *resolverFake, string) {
	t.Helper()
	clock := &fakeClock{t: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)}
	g := newGuardianFake()
	c, runID := enabledGuardianController(t, g, clock, cyclicResolver("a", "free"), testGuardian())
	rf := &resolverFake{guardianFake: g}
	c.SetRuntime(rf)
	c.mu.Lock()
	r := c.runs[runID]
	r.plan.Goal = "ship the thing"
	r.plan.Constraints = []string{"no new deps"}
	r.state = StateActive
	c.mu.Unlock()
	return c, rf, runID
}

func TestResolverSpawnedWithRightPrompt(t *testing.T) {
	c, rf, runID := resolverSetup(t)
	ok, err := c.SpawnResolver(context.Background(), ResolverRequest{
		RunID: runID, TaskID: "t1", Branch: "w/a", Class: BlockerRedGate, Detail: "TestX failing"})
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, rf.spawned, 1)
	s := rf.spawned[0]
	require.Equal(t, "w/a", s.Branch)
	require.Equal(t, 1, s.Attempt)
	for _, want := range []string{"ship the thing", "no new deps", "TestX failing", "red_gate", "NEVER wait for a human", "wd job done", "Do NOT merge"} {
		require.Contains(t, s.Prompt, want)
	}
	require.Contains(t, strings.Join(rf.audits, ","), "autopilot.resolver_spawned:resolver-1")
	require.Equal(t, 1, c.Status().Runs[0].ResolverAttempts["w/a"])
}

func TestResolverCapParksRun(t *testing.T) {
	c, rf, runID := resolverSetup(t)
	req := ResolverRequest{RunID: runID, TaskID: "t1", Branch: "w/a", Class: BlockerRedGate, Detail: "red"}
	for i := 0; i < MaxResolverAttempts; i++ {
		ok, err := c.SpawnResolver(context.Background(), req)
		require.NoError(t, err)
		require.True(t, ok)
	}
	ok, err := c.SpawnResolver(context.Background(), req)
	require.NoError(t, err)
	require.False(t, ok, "fourth attempt must not spawn")
	require.Len(t, rf.spawned, MaxResolverAttempts)
	st := c.Status().Runs[0]
	require.Contains(t, st.NeedsAttention, string(KindResolverExhausted))
	require.Equal(t, MaxResolverAttempts, st.ResolverAttempts["w/a"])

	// A different PR has its own budget.
	req.Branch = "w/b"
	c.mu.Lock()
	c.runs[runID].needsAttention = ""
	c.mu.Unlock()
	ok, _ = c.SpawnResolver(context.Background(), req)
	require.True(t, ok)
}

func TestFixLoopCapExitSpawnsResolver(t *testing.T) {
	c, fr := fixSetup(t)
	c.SetFixPolicy(FixPolicy{MaxFixes: 1, MaxRedSHAs: 99})
	for _, sha := range []string{"s1", "s2"} {
		runFix(c, fr, redFix(sha, LandOwner{TaskID: "t1", WorkerID: "w1"}))
	}
	require.Len(t, fr.resolved, 1)
	require.Equal(t, BlockerRedGate, fr.resolved[0].Class)
	require.Contains(t, fr.audits, "autopilot.resolver_spawned")
}
