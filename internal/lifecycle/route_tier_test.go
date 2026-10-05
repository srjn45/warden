package lifecycle

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/fastbrain"
	"github.com/srjn45/warden/internal/router"
)

type routeCfg struct {
	FakeConfig
	on bool
}

func (c *routeCfg) GetRouteTierUseFastBrain() bool { return c.on }

// routeFB is a fastbrain.Engine returning a canned route_tier reply with the
// confidence the real engine would lift from the JSON.
type routeFB struct {
	fastbrain.Engine
	tier   string
	conf   float64
	status fastbrain.Status
	raw    string
	err    error
	calls  []fastbrain.Request
}

func (f *routeFB) Decide(_ context.Context, req fastbrain.Request) (fastbrain.Response, error) {
	f.calls = append(f.calls, req)
	if f.err != nil {
		return fastbrain.Response{}, f.err
	}
	st := f.status
	if st == "" {
		st = fastbrain.StatusOK
	}
	r := fastbrain.Response{Kind: req.Kind, Status: st, Confidence: f.conf}
	if st == fastbrain.StatusOK {
		raw := f.raw
		if raw == "" {
			raw = `{"tier":"` + f.tier + `"}`
		}
		r.Output = fastbrain.Output{Raw: raw, Parsed: []byte(raw)}
	}
	return r, nil
}

// routedTier mirrors Spawn's call sequence (applyRouteTier → resolveSpawnTarget)
// and returns the tier the resolver was asked for plus the recorded decision.
func routedTier(t *testing.T, on bool, fb *routeFB, req SpawnRequest) (string, *routeTierDecision) {
	t.Helper()
	cr := &stubResolveCapture{res: &router.Resolution{BackendID: "claude", ModelID: "m"}}
	lc := New(&FakeRunner{}, &routeCfg{on: on})
	lc.Resolver = cr
	if fb != nil {
		lc.FastBrain = fb
	}
	d := lc.applyRouteTier(context.Background(), &req)
	_, _, err := lc.resolveSpawnTarget(context.Background(), req.Role, req.Task, req.Tier, req.Backend, req.Model)
	require.NoError(t, err)
	return string(cr.opts.Tier), d
}

func TestRouteTierPrecedenceMatrix(t *testing.T) {
	confident := func() *routeFB { return &routeFB{tier: "tier-3", conf: 0.95} }
	base := SpawnRequest{Prompt: "redesign the storage layer"}

	t.Run("setting on + confident decision changes the tier", func(t *testing.T) {
		fb := confident()
		tier, d := routedTier(t, true, fb, base)
		require.Equal(t, "tier-3", tier)
		require.Equal(t, &routeTierDecision{Tier: "tier-3", Confidence: 0.95, Applied: true}, d)
		require.Len(t, fb.calls, 1)
		require.Equal(t, fastbrain.KindRouteTier, fb.calls[0].Kind)
	})

	pins := map[string]func(*SpawnRequest){
		"explicit tier": func(r *SpawnRequest) { r.Tier = "tier-1" },
		"task":          func(r *SpawnRequest) { r.Task = "development" },
		"model":         func(r *SpawnRequest) { r.Model, r.Backend = "opus", "claude" },
		"ai_cli":        func(r *SpawnRequest) { r.AiCli = "claude" },
		"backend":       func(r *SpawnRequest) { r.Backend = "claude" },
		"role":          func(r *SpawnRequest) { r.Role = "worker" },
	}
	for name, pin := range pins {
		t.Run(name+" beats the router", func(t *testing.T) {
			req := base
			pin(&req)
			fb := confident()
			got, d := routedTier(t, true, fb, req)
			want, _ := routedTier(t, false, nil, req) // today's result
			require.Equal(t, want, got)
			require.Nil(t, d)
			require.Empty(t, fb.calls, "router must not even be asked when something is pinned")
		})
	}
}

func TestRouteTierFallsBackToTodaysResolution(t *testing.T) {
	base := SpawnRequest{Prompt: "fix typo"}
	today, _ := routedTier(t, false, nil, base)
	require.Empty(t, today)

	cases := map[string]struct {
		on bool
		fb *routeFB
	}{
		"setting off":    {false, &routeFB{tier: "tier-1", conf: 0.99}},
		"low confidence": {true, &routeFB{tier: "tier-1", conf: 0.79}},
		"timeout":        {true, &routeFB{status: fastbrain.StatusTimeout}},
		"invalid json":   {true, &routeFB{status: fastbrain.StatusInvalidJSON}},
		"runner error":   {true, &routeFB{err: errors.New("boom")}},
		"unknown tier":   {true, &routeFB{tier: "heavy", conf: 0.99}},
		"no engine":      {true, nil},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, d := routedTier(t, c.on, c.fb, base)
			require.Equal(t, today, got)
			if d != nil { // low confidence is recorded but not applied
				require.False(t, d.Applied)
			}
		})
	}
}

func TestRouteTierDecisionRecordedWhenNotApplied(t *testing.T) {
	_, d := routedTier(t, true, &routeFB{tier: "tier-1", conf: 0.5}, SpawnRequest{Prompt: "x"})
	require.NotNil(t, d)
	ev := d.event()
	require.Equal(t, "tier-route", ev.Type)
	require.Contains(t, ev.Detail, "suggested=tier-1")
	require.Contains(t, ev.Detail, "confidence=0.50")
	require.Contains(t, ev.Detail, "applied=false")
}

func TestRouteTierEmptyPromptSkipped(t *testing.T) {
	fb := &routeFB{tier: "tier-1", conf: 1}
	_, d := routedTier(t, true, fb, SpawnRequest{})
	require.Nil(t, d)
	require.Empty(t, fb.calls)
}
