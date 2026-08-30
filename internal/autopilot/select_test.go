package autopilot

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/backendstore"
	"github.com/srjn45/warden/internal/router"
	"github.com/stretchr/testify/require"
)

// fakeResolver is a minimal router.Resolver-shaped stub for select/guardian tests.
// It returns a fixed BackendID or an error on each call, and counts invocations.
type fakeResolver struct {
	backendID string
	tier      backendstore.ModelTier
	err       error
	calls     int
}

func (f *fakeResolver) Resolve(_ context.Context, opts router.ResolveOptions) (*router.Resolution, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return &router.Resolution{BackendID: f.backendID, Tier: f.tier}, nil
}

// roundRobinResolver cycles through a list of backends on successive Resolve calls,
// wrapping back to the first. It is used in guardian tests to simulate walking the
// cost-tier ladder: the first call returns the "free" backend, the second returns
// the "subscription" backend (after the free one is added to the exclude map and
// selectBrain detects that and returns no selection, triggering the next round).
type roundRobinResolver struct {
	backends []string
	tiers    []backendstore.ModelTier
	i        int
}

func (r *roundRobinResolver) Resolve(_ context.Context, _ router.ResolveOptions) (*router.Resolution, error) {
	if len(r.backends) == 0 {
		return nil, router.ErrNoCandidate
	}
	idx := r.i % len(r.backends)
	r.i++
	return &router.Resolution{BackendID: r.backends[idx], Tier: r.tiers[idx]}, nil
}

// cyclicResolver always returns the same backend — used where a single backend is
// all that is needed and roundRobin would be overkill.
func cyclicResolver(backendID string, tier backendstore.ModelTier) Resolver {
	return &fakeResolver{backendID: backendID, tier: tier}
}

// TestSelectBrainUsesResolver proves selectBrain delegates to the injected Resolver
// with Role:"autopilot" and returns its BackendID + Tier.
func TestSelectBrainUsesResolver(t *testing.T) {
	c := NewController(ControllerConfig{
		Resolver: &fakeResolver{backendID: "antigravity", tier: backendstore.Tier1},
	}, &fakeEnv{})

	sel := c.selectBrain(nil)
	require.True(t, sel.OK)
	require.Equal(t, "antigravity", sel.Backend)
	require.Equal(t, string(backendstore.Tier1), sel.Tier)
}

// TestSelectBrainResolverError proves a resolver error yields no selection.
func TestSelectBrainResolverError(t *testing.T) {
	c := NewController(ControllerConfig{
		Resolver: &fakeResolver{err: errors.New("resolver down")},
	}, &fakeEnv{})

	sel := c.selectBrain(nil)
	require.False(t, sel.OK)
}

// TestSelectBrainNilResolver proves that with no resolver injected the controller
// yields the daemon-default selection (Backend:"", Tier:free, OK:true), preserving
// the inert-core / unit-test path.
func TestSelectBrainNilResolver(t *testing.T) {
	c := NewController(ControllerConfig{}, &fakeEnv{})
	sel := c.selectBrain(nil)
	require.True(t, sel.OK)
	require.Equal(t, "", sel.Backend)
	require.Equal(t, tierFree, sel.Tier)
}

// TestSelectBrainNilResolverDefaultExcluded proves that once the daemon-default ("")
// is excluded (already tried), the nil-resolver path returns no selection — the
// guardian then enters backoff rather than looping forever.
func TestSelectBrainNilResolverDefaultExcluded(t *testing.T) {
	c := NewController(ControllerConfig{}, &fakeEnv{})
	sel := c.selectBrain(map[string]bool{"": true})
	require.False(t, sel.OK)
}

// TestSelectBrainExcludeMap proves that the exclude map prevents the resolver's
// chosen backend from being re-selected (guardian rotate-down semantics).
func TestSelectBrainExcludeMap(t *testing.T) {
	r := &fakeResolver{backendID: "claude", tier: backendstore.Tier2}
	c := NewController(ControllerConfig{Resolver: r}, &fakeEnv{})

	// First call: no exclusion — claude is returned.
	sel := c.selectBrain(nil)
	require.True(t, sel.OK)
	require.Equal(t, "claude", sel.Backend)

	// Second call: claude excluded — no selection (resolver only offered claude).
	sel2 := c.selectBrain(map[string]bool{"claude": true})
	require.False(t, sel2.OK)
}

// TestSelectWorkerBackendUsesResolver proves SelectWorkerBackend calls the Resolver
// with Role:"worker" and returns its BackendID.
func TestSelectWorkerBackendUsesResolver(t *testing.T) {
	dir := t.TempDir()
	plan := writePlan(t, dir, "plan.yaml", "ship it")
	rt := newFakeRuntime()
	r := &fakeResolver{backendID: "antigravity", tier: backendstore.Tier1}
	c := NewController(ControllerConfig{
		Plans:    []string{plan},
		BaseDir:  dir,
		Resolver: r,
	}, &fakeEnv{})
	c.SetRuntime(rt)

	st, err := c.Enable(context.Background(), "")
	require.NoError(t, err)
	runID := st.Runs[0].RunID

	// Change the fake to return a worker backend so we can distinguish the call.
	r.backendID = "claude"
	backend, ok := c.SelectWorkerBackend(runID)
	require.True(t, ok)
	require.Equal(t, "claude", backend)
}

// TestSelectWorkerBackendNoResolver proves SelectWorkerBackend returns false when no
// resolver is injected (inert-core mode).
func TestSelectWorkerBackendNoResolver(t *testing.T) {
	dir := t.TempDir()
	plan := writePlan(t, dir, "plan.yaml", "ship it")
	rt := newFakeRuntime()
	c := NewController(ControllerConfig{
		Plans:   []string{plan},
		BaseDir: dir,
	}, &fakeEnv{})
	c.SetRuntime(rt)

	st, err := c.Enable(context.Background(), "")
	require.NoError(t, err)
	runID := st.Runs[0].RunID

	_, ok := c.SelectWorkerBackend(runID)
	require.False(t, ok, "no resolver ⇒ no worker backend selected")
}

// TestMarkBackendLimitedStillWorks proves MarkBackendLimited continues to update
// the tierstate (used by enterBackoff's earliestReset heuristic) even after the
// resolver migration. It is a no-op on selection logic (resolver owns that), but the
// backoff-wake time still reads tierstate.earliestReset.
func TestMarkBackendLimitedStillWorks(t *testing.T) {
	c := NewController(ControllerConfig{}, &fakeEnv{})
	limit := time.Now().Add(30 * time.Minute)
	// Must not panic and must record the limit so the guardian can read earliestReset.
	c.MarkBackendLimited("some-backend", limit)
	reset, ok := c.tierstate.earliestReset()
	require.True(t, ok)
	require.Equal(t, limit, reset)
}
