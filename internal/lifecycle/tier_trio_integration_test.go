package lifecycle

import (
	"context"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/backendstore"
	"github.com/srjn45/warden/internal/router"
	"github.com/stretchr/testify/require"
)

// This is the cross-cutting end-to-end test for the "tier trio" (task->tier source
// + role tier defaults + resolver precedence from Job 1; first-spawn routed through
// the resolver from Job 2). The per-piece behavior is unit-tested in those jobs
// (router/resolver_test.go, lifecycle/spawn_resolve_test.go); this test wires the
// REAL pieces together — a real backendstore, a real *router.Resolver, and a real
// Lifecycle.Spawn (fake command runner, no tmux/git) — and asserts that the
// backend+model the resolver picks actually lands on the spawned session record.

// tierTrioNow is the fixed clock the fixture and resolver share so antigravity's
// recorded daily-window usage stays active at resolution time (same calendar day),
// keeping headroom — and therefore the winner — deterministic across any wall clock.
var tierTrioNow = time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)

// newTierTrioStore seeds a backendstore in which, across ALL three tiers, claude is
// the quota-headroom winner and antigravity a lower-headroom (but still eligible)
// runner-up. Only claude and antigravity get backend rows, so cursor/codex seed
// models are auto-ineligible ("backend not registered") and never compete. claude
// carries exactly one enabled model per tier in the seed catalog
// (claude-opus / sonnet / claude-3-5-haiku), so the winning MODEL is
// deterministic — there is no round-robin among tied same-backend models.
func newTierTrioStore(t *testing.T) *backendstore.Store {
	t.Helper()
	s, err := backendstore.NewStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	// claude + antigravity: installed, enabled, subscription-tier candidates. Their
	// seed models (one per tier for claude, several for antigravity) come from the
	// store's default catalog.
	for _, id := range []string{"claude", "antigravity"} {
		require.NoError(t, s.Upsert(backendstore.Backend{
			ID: id, Installed: true, Enabled: true, Tier: backendstore.TierSubscription,
		}))
	}

	// Differing quota headroom: claude 100% (no usage recorded → full headroom),
	// antigravity 50% (500k of its 1M daily window). antigravity stays well under the
	// 90% threshold, so it is eligible — it simply loses to claude on headroom. This
	// gap is what makes claude the deterministic winner (equal headroom would tie and
	// round-robin across the two backends).
	require.NoError(t, s.RecordQuotaUsage("antigravity", 500000, "claude-sonnet-4-6", tierTrioNow))
	return s
}

// TestTierTrioEndToEnd drives real spawns through a real resolver over a real store
// and asserts the resolved backend+model on the session record.
func TestTierTrioEndToEnd(t *testing.T) {
	// newLC builds a Lifecycle with a fake runner (tmux/git are no-ops) and, when
	// asked, a real resolver over a fresh tier-trio store.
	newLC := func(t *testing.T, withResolver bool) *Lifecycle {
		lc := New(&FakeRunner{}, &FakeConfig{})
		if withResolver {
			lc.Resolver = router.NewResolver(newTierTrioStore(t)).
				WithNow(func() time.Time { return tierTrioNow })
		}
		return lc
	}

	t.Run("role resolves to the role default tier and the headroom winner", func(t *testing.T) {
		lc := newLC(t, true)
		s, err := lc.Spawn(context.Background(), SpawnRequest{Role: "orchestrator", Cwd: t.TempDir()})
		require.NoError(t, err)
		// orchestrator → tier-1 (its seeded role default). claude wins tier-1 on
		// headroom (1.0) over antigravity (0.5), and claude's only tier-1 model is
		// claude-opus.
		require.Equal(t, "claude", s.Backend)
		require.Equal(t, "claude-opus", s.Model)
	})

	t.Run("task tier overrides the role tier", func(t *testing.T) {
		lc := newLC(t, true)
		// orchestrator's role tier is 1, but the development task is tier-2, and
		// task.TierFor wins over the role tier — so this resolves in tier-2.
		s, err := lc.Spawn(context.Background(), SpawnRequest{
			Role: "orchestrator", Task: "development", Cwd: t.TempDir(),
		})
		require.NoError(t, err)
		require.Equal(t, "claude", s.Backend)
		require.Equal(t, "sonnet", s.Model) // claude's tier-2 model
	})

	t.Run("explicit tier overrides both role and task", func(t *testing.T) {
		lc := newLC(t, true)
		// role tier-1 and task tier-2 are both present, but an explicit tier-3 wins
		// outright (explicit tier > task > role).
		s, err := lc.Spawn(context.Background(), SpawnRequest{
			Role: "orchestrator", Task: "development", Tier: "tier-3", Cwd: t.TempDir(),
		})
		require.NoError(t, err)
		require.Equal(t, "claude", s.Backend)
		require.Equal(t, "claude-3-5-haiku", s.Model) // claude's tier-3 model
	})

	t.Run("pinned backend and model bypass the resolver", func(t *testing.T) {
		lc := newLC(t, true)
		// Pin the LOWER-headroom backend: the resolver would have chosen claude, so
		// getting antigravity back proves the router was never consulted for a pin.
		s, err := lc.Spawn(context.Background(), SpawnRequest{
			Role: "orchestrator", Backend: "antigravity", Model: "claude-sonnet-4-6", Cwd: t.TempDir(),
		})
		require.NoError(t, err)
		require.Equal(t, "antigravity", s.Backend)
		require.Equal(t, "claude-sonnet-4-6", s.Model)
	})

	t.Run("no resolver wired still spawns (degraded path)", func(t *testing.T) {
		lc := newLC(t, false) // lc.Resolver == nil
		s, err := lc.Spawn(context.Background(), SpawnRequest{Role: "orchestrator", Cwd: t.TempDir()})
		require.NoError(t, err)
		require.NotNil(t, s)
		// A first spawn never hard-fails on resolution: with no resolver it degrades
		// to the request values — the config default backend (empty ⇒ claude) and no
		// model pin.
		require.Equal(t, "", s.Backend)
		require.Equal(t, "", s.Model)
	})
}
