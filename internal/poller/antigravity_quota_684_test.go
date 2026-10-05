package poller

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/srjn45/warden/internal/agentbackend/backends"
	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/capacity"
	"github.com/srjn45/warden/internal/store"
)

const agy684Pane = "⚠ Individual quota reached. Please upgrade your subscription to increase your limits. Resets in 10m5s.\n" +
	"Error ID: 579fd7be-0000-0000-0000-000000000000\n\n" +
	"────────────────\n>\n────────────────\n" +
	"? for shortcuts                                   Gemini 3.8 Flash · medium\n"

// TestTick_Antigravity684_RateLimitedAndObserved drives the real tick: the #684
// pane classifies as rate_limited, fires the observation (with the banner in the
// fresh excerpt, which feeds the scheduler → recovery coordinator), and reports
// the observed Gemini bucket for re-binding.
func TestTick_Antigravity684_RateLimitedAndObserved(t *testing.T) {
	binding := &capacity.QuotaBinding{
		Domain:           capacity.CapacityDomain{Provider: "antigravity", AiCli: "antigravity", Route: "claude-opus-4-6-thinking", BucketKeys: []string{"non-gemini"}},
		MandatoryBuckets: []string{"non-gemini"},
	}
	d := &stubDeps{
		sessions: []*agentstore.Agent{{ID: "A-1", TmuxSession: "A-1", AiCli: "antigravity", Status: store.StatusIdle, QuotaBinding: binding}},
		alive:    map[string]bool{"A-1": true},
		panes:    map[string]string{"A-1": agy684Pane},
		updates:  map[string]store.Status{},
	}
	p := New(d, 5*time.Minute)
	p.Backend = func(*agentstore.Agent) agentbackend.Backend { return backends.Antigravity{} }
	var obs []RateLimitObservation
	p.OnRateLimitObservation = func(o RateLimitObservation) { obs = append(obs, o) }
	var gotModel, gotScope string
	p.OnObservedQuotaScope = func(_ *agentstore.Agent, m, s string) { gotModel, gotScope = m, s }

	require.NoError(t, p.tick(context.Background()))
	require.Equal(t, store.StatusRateLimited, d.updates["A-1"])
	require.Len(t, obs, 1)
	require.Contains(t, obs[0].FreshExcerpt, "Resets in 10m5s")
	require.Equal(t, "Gemini 3.8 Flash", gotModel)
	require.Equal(t, "gemini", gotScope)
}

func TestTick_ObservedScopeMatchingBindingIsSilent(t *testing.T) {
	binding := &capacity.QuotaBinding{
		Domain:           capacity.CapacityDomain{Provider: "antigravity", AiCli: "antigravity", BucketKeys: []string{"gemini"}},
		MandatoryBuckets: []string{"gemini"},
	}
	d := &stubDeps{
		sessions: []*agentstore.Agent{{ID: "A-1", TmuxSession: "A-1", AiCli: "antigravity", Status: store.StatusIdle, QuotaBinding: binding}},
		alive:    map[string]bool{"A-1": true},
		panes:    map[string]string{"A-1": agy684Pane},
		updates:  map[string]store.Status{},
	}
	p := New(d, 5*time.Minute)
	p.Backend = func(*agentstore.Agent) agentbackend.Backend { return backends.Antigravity{} }
	called := false
	p.OnObservedQuotaScope = func(*agentstore.Agent, string, string) { called = true }
	require.NoError(t, p.tick(context.Background()))
	require.False(t, called)
}
