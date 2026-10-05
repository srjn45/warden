package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/backendusage"
	"github.com/srjn45/warden/internal/capacity"
	"github.com/srjn45/warden/internal/poller"
	"github.com/srjn45/warden/internal/store"
)

// The exact pane tail from GitHub #684.
const agy684Pane = "⚠ Individual quota reached. Please upgrade your subscription to increase your limits. Resets in 10m5s.\n" +
	"Error ID: 579fd7be-0000-0000-0000-000000000000\n\n" +
	"────────────────\n>\n────────────────\n" +
	"? for shortcuts                                   Gemini 3.8 Flash · medium\n"

// TestRateLimitAntigravityBannerThroughSchedulerToRecovery traces the #684 banner
// from the poller's observation through RateLimitScheduler into the backend
// recovery coordinator: the relative reset is parsed, the agent is claimed by
// recovery (not the legacy resume timer) and hot-swapped, with the
// backend_recovery_started event recorded.
func TestRateLimitAntigravityBannerThroughSchedulerToRecovery(t *testing.T) {
	agy, err := agentbackend.Get("antigravity")
	require.NoError(t, err)
	limited, _, _ := agy.(agentbackend.RateLimitDetector).DetectRateLimit(agy684Pane)
	require.True(t, limited)

	c, st, life := recoveryFixture(t, map[string][]backendusage.Limit{
		"antigravity": {{ID: "weekly", Scope: "weekly", Label: "Weekly", UsedPercent: used(100)}},
		"claude":      {{ID: "weekly", Scope: "weekly", Label: "Weekly", UsedPercent: used(20)}},
	})
	require.NoError(t, st.Update(context.Background(), "agent-1", func(a *agentstore.Agent) error {
		a.AiCli, a.Model = "antigravity", "antigravity-model"
		return nil
	}))

	sched := NewRateLimitScheduler(nil, st, 30*time.Minute, 6*time.Hour, time.Minute, true, "")
	sched.BackendResolver = func(s *agentstore.Agent) agentbackend.Backend {
		b, _ := agentbackend.Get(s.AiCli)
		return b
	}
	var until time.Time
	sched.OnHardLimit = func(sess *agentstore.Agent, u time.Time) bool {
		until = u
		return c.OnHardLimit(sess, u)
	}

	before := time.Now()
	sched.OnRateLimitObservation(poller.NewRateLimitObservation("agent-1", agy684Pane))

	require.WithinDuration(t, before.Add(10*time.Minute+5*time.Second+time.Minute), until, 10*time.Second,
		"relative reset (+1m scheduler buffer) must be parsed rather than falling back to the retry interval")
	require.Eventually(t, func() bool {
		s := st.snapSession("agent-1")
		return s != nil && s.BackendRecovery != nil && s.AiCli != "antigravity"
	}, time.Second, 5*time.Millisecond)
	require.NotEmpty(t, life.swaps)
	var started bool
	for _, ev := range st.snapSession("agent-1").Events {
		if ev.Type == "backend_recovery_started" {
			started = true
		}
	}
	require.True(t, started, "recovery_started event must be recorded")
}

func TestQuotaRebinder(t *testing.T) {
	ctx := context.Background()
	st := newFakeStore()
	orig := &capacity.QuotaBinding{
		Domain:           capacity.CapacityDomain{Provider: "antigravity", AiCli: "antigravity", AccountFingerprint: "sha256:x", Route: "claude-opus-4-6-thinking", BucketKeys: []string{"non-gemini"}},
		MandatoryBuckets: []string{"non-gemini"},
	}
	require.NoError(t, st.Insert(ctx, &agentstore.Agent{ID: "a1", AiCli: "antigravity", Status: store.StatusWorking, QuotaBinding: orig}))
	require.NoError(t, st.Insert(ctx, &agentstore.Agent{ID: "legacy", AiCli: "antigravity", Status: store.StatusWorking}))

	rebind := NewQuotaRebinder(st)
	rebind(&agentstore.Agent{ID: "legacy"}, "Gemini 3.8 Flash", "gemini")
	require.Nil(t, st.snapSession("legacy").QuotaBinding, "unbound legacy agents stay unbound")

	rebind(&agentstore.Agent{ID: "a1"}, "Gemini 3.8 Flash", "gemini")
	got := st.snapSession("a1")
	require.Equal(t, []string{"gemini"}, got.QuotaBinding.MandatoryBuckets)
	require.Equal(t, []string{"gemini"}, got.QuotaBinding.Domain.BucketKeys)
	require.Equal(t, "sha256:x", got.QuotaBinding.Domain.AccountFingerprint)
	require.Equal(t, "claude-opus-4-6-thinking", got.QuotaBinding.Domain.Route)
	require.Len(t, got.Events, 1)
	require.Equal(t, "quota_rebound", got.Events[0].Type)
	require.Contains(t, got.Events[0].Detail, "Gemini 3.8 Flash")

	rebind(&agentstore.Agent{ID: "a1"}, "Gemini 3.8 Flash", "gemini")
	require.Len(t, st.snapSession("a1").Events, 1, "already-bound scope is a no-op")
}
