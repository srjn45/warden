package lifecycle

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/srjn45/warden/internal/backendstore"
	"github.com/srjn45/warden/internal/router"
	"github.com/stretchr/testify/require"
)

func seededCatalogStore(t *testing.T) *backendstore.Store {
	t.Helper()
	s, err := backendstore.NewStore(t.TempDir())
	require.NoError(t, err)
	for _, id := range []string{"claude", "antigravity", "cursor", "codex"} {
		require.NoError(t, s.Upsert(backendstore.Backend{ID: id, Installed: true, Enabled: true, Tier: backendstore.TierSubscription}))
	}
	return s
}

// discoveryProbe wraps a registered backend and counts any catalog discovery or
// live-menu call. A cached launch must never reach it.
type discoveryProbe struct {
	agentbackend.Backend
	calls *atomic.Int64
}

func (p discoveryProbe) ListModels() ([]string, bool) { p.calls.Add(1); return nil, false }
func (p discoveryProbe) ResolveScope(context.Context) (agentbackend.CatalogScope, error) {
	p.calls.Add(1)
	return agentbackend.CatalogScope{}, nil
}
func (p discoveryProbe) DiscoverModels(context.Context, agentbackend.CatalogScope) agentbackend.CatalogObservation {
	p.calls.Add(1)
	return agentbackend.CatalogObservation{}
}

func probeAllBackends(t *testing.T) *atomic.Int64 {
	t.Helper()
	calls := &atomic.Int64{}
	for _, id := range agentbackend.IDs() {
		orig, err := agentbackend.Get(id)
		require.NoError(t, err)
		agentbackend.Register(discoveryProbe{Backend: orig, calls: calls})
		t.Cleanup(func() { agentbackend.Register(orig) })
	}
	return calls
}

// TestCachedLaunchResolutionNeverRunsDiscovery proves launch routing reads only
// stored observations: every registered backend is wrapped by a call counter and
// stays at zero across pinned, unpinned and rejected-candidate resolutions.
func TestCachedLaunchResolutionNeverRunsDiscovery(t *testing.T) {
	calls := probeAllBackends(t)
	s := seededCatalogStore(t)
	defer s.Close()
	lc := New(&FakeRunner{}, &FakeConfig{})
	lc.Resolver = router.NewResolver(s)
	ctx := context.Background()

	_, _, err := lc.resolveSpawnTarget(ctx, "worker", "development", "tier-1", "", "")
	require.NoError(t, err)
	_, _, err = lc.resolveSpawnTarget(ctx, "worker", "development", "tier-1", "codex", "")
	require.NoError(t, err)
	_, _, err = lc.resolveSpawnTarget(ctx, "", "", "", "codex", "gpt-5.5")
	require.NoError(t, err)
	require.Zero(t, calls.Load(), "launch resolution must not run catalog discovery or ListModels")
}

// TestStartupResolutionExactPinNeverSubstitutedAndExhaustionFailsTyped uses the
// real resolver over a launch-rejected catalog. An exact pin is returned
// unchanged; an unpinned/aicli-pinned launch whose only candidates are rejected
// fails with the typed exhaustion error and never degrades to a default model or another tier.
func TestStartupResolutionExactPinNeverSubstitutedAndExhaustionFailsTyped(t *testing.T) {
	s := seededCatalogStore(t)
	defer s.Close()
	now := time.Now().UTC()
	require.NoError(t, s.PutCatalogScope(backendstore.ActiveCatalogScope{AiCli: "codex", ActiveScopeFP: "scope"}))
	models, err := s.ListModels(backendstore.ModelTier(""))
	require.NoError(t, err)
	var codexModels int
	for _, m := range models {
		if m.BackendID != "codex" {
			continue
		}
		codexModels++
		require.NoError(t, s.PutCatalogObservation(backendstore.StoredObservation{
			AiCli: "codex", ScopeFP: "scope", ModelID: m.ModelID, Availability: backendstore.AvailabilityUnavailable,
			LaunchRejectionReason: "invalid_model", LaunchRejectedUntil: now.Add(time.Hour),
		}))
	}
	require.NotZero(t, codexModels)

	lc := New(&FakeRunner{}, &FakeConfig{})
	lc.Resolver = router.NewResolver(s)
	ctx := context.Background()

	b, m, err := lc.resolveSpawnTarget(ctx, "worker", "", "tier-1", "codex", "gpt-5.5")
	require.NoError(t, err)
	require.Equal(t, [2]string{"codex", "gpt-5.5"}, [2]string{b, m}, "exact pin must not be substituted")

	_, _, err = lc.resolveSpawnTarget(ctx, "worker", "", "tier-1", "codex", "")
	require.ErrorIs(t, err, router.ErrAllExhausted)
}
