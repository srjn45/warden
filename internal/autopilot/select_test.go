package autopilot

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeLadderSource is a store-shaped TierLadderSource for exercising the store-driven
// selection path (docs/specs/2026-08-06-backend-registry.md §8) without a real store.
type fakeLadderSource struct {
	ladder    BackendLadder
	allowPaid bool
	err       error
}

func (f fakeLadderSource) TierLadder() (BackendLadder, bool, error) {
	return f.ladder, f.allowPaid, f.err
}

// TestSelectBackendStoreDriven proves selectBackend reads its tiers + paid gate from
// the source (the registry store in production), not a config ladder, and that a
// source error degrades to the daemon default rather than risking a paid pick.
func TestSelectBackendStoreDriven(t *testing.T) {
	ladder := BackendLadder{Free: []string{"antigravity"}, Subscription: []string{"claude"}, PayPerUse: []string{"gpt"}}

	// Gate off (store Settings.AllowPaidAutopilot=false): free wins.
	sel := selectBackend(fakeLadderSource{ladder: ladder}, nil, nil)
	require.True(t, sel.OK)
	require.Equal(t, "antigravity", sel.Backend)
	require.Equal(t, tierFree, sel.Tier)

	// Gate ON, whole free+sub tier excluded ⇒ pay_per_use becomes selectable.
	sel = selectBackend(fakeLadderSource{ladder: ladder, allowPaid: true}, nil,
		map[string]bool{"antigravity": true, "claude": true})
	require.True(t, sel.OK)
	require.Equal(t, "gpt", sel.Backend)
	require.Equal(t, tierPayPerUse, sel.Tier)

	// Same, gate OFF ⇒ the gate-only signal, no selection.
	sel = selectBackend(fakeLadderSource{ladder: ladder}, nil,
		map[string]bool{"antigravity": true, "claude": true})
	require.False(t, sel.OK)
	require.True(t, sel.GateOnly)

	// A registry read error degrades to the daemon default ("") — never a paid guess.
	sel = selectBackend(fakeLadderSource{ladder: ladder, allowPaid: true, err: errors.New("store down")}, nil, nil)
	require.True(t, sel.OK)
	require.Equal(t, "", sel.Backend)
	require.Equal(t, tierFree, sel.Tier)

	// …and once "" is excluded (already tried), even the default collapses to none.
	sel = selectBackend(fakeLadderSource{err: errors.New("store down")}, nil, map[string]bool{"": true})
	require.False(t, sel.OK)
}

// TestControllerTierSourcePrefersStore proves the Controller uses an injected
// registry source over the config-derived fallback, and falls back when none is set.
func TestControllerTierSourcePrefersStore(t *testing.T) {
	// Injected store source wins over the config ladder.
	c := NewController(ControllerConfig{
		LadderSource: fakeLadderSource{ladder: BackendLadder{Free: []string{"from-store"}}},
		Backends:     BackendLadder{Free: []string{"from-config"}},
	}, &fakeEnv{})
	l, _, err := c.tierSource().TierLadder()
	require.NoError(t, err)
	require.Equal(t, []string{"from-store"}, l.Free)

	// No source ⇒ the config ladder is wrapped as the fallback source.
	c = NewController(ControllerConfig{Backends: BackendLadder{Free: []string{"from-config"}}, AllowPayPerUse: true}, &fakeEnv{})
	l, allow, err := c.tierSource().TierLadder()
	require.NoError(t, err)
	require.Equal(t, []string{"from-config"}, l.Free)
	require.True(t, allow)
}

// TestSelectBackendPermutations walks the cost-tier selection loop (autopilot.md
// §7) across tier / gate / limited combinations.
func TestSelectBackendPermutations(t *testing.T) {
	ladder := BackendLadder{
		Free:         []string{"antigravity", "codex"},
		Subscription: []string{"claude"},
		PayPerUse:    []string{"gpt"},
	}
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name         string
		ladder       BackendLadder
		allowPPU     bool
		exclude      []string
		limited      map[string]time.Duration // backend → limited-for
		wantBackend  string
		wantTier     string
		wantOK       bool
		wantGateOnly bool
	}{
		{
			name: "first free wins", ladder: ladder,
			wantBackend: "antigravity", wantTier: tierFree, wantOK: true,
		},
		{
			name: "free limited ⇒ next free", ladder: ladder,
			limited:     map[string]time.Duration{"antigravity": time.Hour},
			wantBackend: "codex", wantTier: tierFree, wantOK: true,
		},
		{
			name: "whole free tier limited ⇒ subscription", ladder: ladder,
			limited:     map[string]time.Duration{"antigravity": time.Hour, "codex": time.Hour},
			wantBackend: "claude", wantTier: tierSubscription, wantOK: true,
		},
		{
			name: "free+sub limited, gate off ⇒ none, gateOnly", ladder: ladder,
			limited:      map[string]time.Duration{"antigravity": time.Hour, "codex": time.Hour, "claude": time.Hour},
			wantOK:       false,
			wantGateOnly: true,
		},
		{
			name: "free+sub limited, gate ON ⇒ pay_per_use", ladder: ladder, allowPPU: true,
			limited:     map[string]time.Duration{"antigravity": time.Hour, "codex": time.Hour, "claude": time.Hour},
			wantBackend: "gpt", wantTier: tierPayPerUse, wantOK: true,
		},
		{
			name: "everything limited, gate ON ⇒ none, not gateOnly", ladder: ladder, allowPPU: true,
			limited: map[string]time.Duration{"antigravity": time.Hour, "codex": time.Hour, "claude": time.Hour, "gpt": time.Hour},
			wantOK:  false,
		},
		{
			name: "exclude free tier ⇒ subscription (rotate-down)", ladder: ladder,
			exclude:     []string{"antigravity", "codex"},
			wantBackend: "claude", wantTier: tierSubscription, wantOK: true,
		},
		{
			name: "no backends configured ⇒ daemon default", ladder: BackendLadder{},
			wantBackend: "", wantTier: tierFree, wantOK: true,
		},
		{
			name: "no backends configured, default already tried ⇒ none", ladder: BackendLadder{},
			exclude: []string{""},
			wantOK:  false,
		},
		{
			name: "pay_per_use only, gate off ⇒ gateOnly", ladder: BackendLadder{PayPerUse: []string{"gpt"}},
			wantOK: false, wantGateOnly: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock := &fakeClock{t: now}
			ts := newTierState(clock.now)
			for b, d := range tc.limited {
				ts.markLimited(b, now.Add(d))
			}
			var exclude map[string]bool
			if tc.exclude != nil {
				exclude = map[string]bool{}
				for _, e := range tc.exclude {
					exclude[e] = true
				}
			}
			got := selectBackend(staticLadder{ladder: tc.ladder, allowPaid: tc.allowPPU}, ts, exclude)
			require.Equal(t, tc.wantOK, got.OK, "OK")
			require.Equal(t, tc.wantBackend, got.Backend, "Backend")
			if tc.wantOK {
				require.Equal(t, tc.wantTier, got.Tier, "Tier")
			}
			require.Equal(t, tc.wantGateOnly, got.GateOnly, "GateOnly")
		})
	}
}

// TestSelectLimitedExpiryClimbsBack proves a limited backend re-qualifies once its
// window elapses, so selection climbs back up the ladder (§7).
func TestSelectLimitedExpiryClimbsBack(t *testing.T) {
	ladder := BackendLadder{Free: []string{"antigravity"}, Subscription: []string{"claude"}}
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: now}
	ts := newTierState(clock.now)

	src := staticLadder{ladder: ladder}
	ts.markLimited("antigravity", now.Add(30*time.Minute))
	require.Equal(t, "claude", selectBackend(src, ts, nil).Backend, "free limited ⇒ subscription")

	clock.t = now.Add(31 * time.Minute)
	sel := selectBackend(src, ts, nil)
	require.Equal(t, "antigravity", sel.Backend, "free re-qualifies after its window elapses")
	require.Equal(t, tierFree, sel.Tier)
}
