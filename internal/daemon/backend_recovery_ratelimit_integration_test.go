package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/agentbackend"
	_ "github.com/srjn45/warden/internal/agentbackend/backends"
	"github.com/srjn45/warden/internal/backendstore"
	"github.com/srjn45/warden/internal/backendusage"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// TestBackendRecoveryOnHardLimitWhenHandoverDisabled verifies that reactive
// recovery is not gated on handover settings — every session gets automatic
// backend switching when rate-limited.
func TestBackendRecoveryOnHardLimitWhenHandoverDisabled(t *testing.T) {
	c, st, life := recoveryFixture(t, map[string][]backendusage.Limit{
		"codex":  {{ID: "weekly", Scope: "weekly", Label: "Weekly", UsedPercent: used(100)}},
		"claude": {{ID: "weekly", Scope: "weekly", Label: "Weekly", UsedPercent: used(20)}},
	})
	require.NoError(t, c.backends.SetHandoverSettings(backendstore.HandoverSettings{Enabled: false}))

	require.True(t, c.OnHardLimit(&store.Session{ID: "agent-1"}, time.Now().Add(time.Hour)))
	require.Eventually(t, func() bool {
		s := st.snapSession("agent-1")
		return s != nil && s.BackendRecovery != nil && len(life.swaps) > 0
	}, time.Second, 5*time.Millisecond)
}

// TestRateLimitCodexPaneThroughSchedulerToRecovery simulates the path from a
// codex rate-limit pane (RateLimitDetector) through RateLimitScheduler.OnTransition
// into BackendRecoveryCoordinator.OnHardLimit and a successful hot-swap.
func TestRateLimitCodexPaneThroughSchedulerToRecovery(t *testing.T) {
	fixture := filepath.Join("..", "agentbackend", "backends", "testdata", "ratelimit", "codex_limited.txt")
	pane, err := os.ReadFile(fixture)
	require.NoError(t, err)

	codex, err := agentbackend.Get("codex")
	require.NoError(t, err)
	rl, ok := codex.(agentbackend.RateLimitDetector)
	require.True(t, ok)
	limited, _, _ := rl.DetectRateLimit(string(pane))
	require.True(t, limited, "codex fixture must conclusively read as rate-limited")

	c, st, life := recoveryFixture(t, map[string][]backendusage.Limit{
		"codex":  {{ID: "weekly", Scope: "weekly", Label: "Weekly", UsedPercent: used(100)}},
		"claude": {{ID: "weekly", Scope: "weekly", Label: "Weekly", UsedPercent: used(20)}},
	})
	life.failures = map[string]error{} // claude launch succeeds if picked first

	sched := NewRateLimitScheduler(nil, st, 30*time.Minute, 6*time.Hour, time.Minute, true, "")
	sched.BackendResolver = func(s *store.Session) agentbackend.Backend {
		b, _ := agentbackend.Get(s.Backend)
		return b
	}
	sched.OnHardLimit = func(sess *store.Session, until time.Time) bool {
		return c.OnHardLimit(sess, until)
	}

	require.NoError(t, st.UpdateStatus(context.Background(), "agent-1", store.StatusWorking))
	sess := st.snapSession("agent-1")
	require.NotNil(t, sess)
	sess.LastPaneExcerpt = string(pane)
	sched.OnTransition(sess, store.StatusWorking, store.StatusRateLimited)

	require.Eventually(t, func() bool {
		snap := st.snapSession("agent-1")
		return snap != nil && snap.BackendRecovery != nil &&
			snap.BackendRecovery.Phase == recoveryStabilizing &&
			snap.Backend != "codex"
	}, time.Second, 5*time.Millisecond)
	require.NotEmpty(t, life.swaps)
}
