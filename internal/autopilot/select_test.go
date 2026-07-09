package autopilot

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

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
			got := selectBackend(tc.ladder, ts, tc.allowPPU, exclude)
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

	ts.markLimited("antigravity", now.Add(30*time.Minute))
	require.Equal(t, "claude", selectBackend(ladder, ts, false, nil).Backend, "free limited ⇒ subscription")

	clock.t = now.Add(31 * time.Minute)
	sel := selectBackend(ladder, ts, false, nil)
	require.Equal(t, "antigravity", sel.Backend, "free re-qualifies after its window elapses")
	require.Equal(t, tierFree, sel.Tier)
}
