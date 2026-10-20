package lifecycle

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// recCfg is a ConfigProvider carrying model_recovery.* values, as config.Config
// does in production. It wraps the rig's own provider so nothing else changes.
type recCfg struct {
	ConfigProvider
	max                       int
	deadline, attempt, reject time.Duration
}

func (c recCfg) ModelRecoveryMaxAttempts() int              { return c.max }
func (c recCfg) ModelRecoveryDeadline() time.Duration       { return c.deadline }
func (c recCfg) ModelRecoveryAttemptTimeout() time.Duration { return c.attempt }
func (c recCfg) ModelRecoveryRejectionTTL() time.Duration   { return c.reject }

func TestModelRecoveryConfigDefaultsWithoutProvider(t *testing.T) {
	r := newRecoveryRig(t, []string{"m"}, map[string]paneScript{"m": {pane: "x"}})
	r.lc.ModelRecovery = ModelRecoveryConfig{} // no override; the rig's provider has no model_recovery keys
	require.Equal(t, ModelRecoveryConfig{MaxAttempts: 3, Deadline: 3 * time.Minute, AttemptTimeout: 60 * time.Second, RejectionTTL: 24 * time.Hour}, r.lc.modelRecoveryConfig())
}

func TestModelRecoveryConfigFromProviderAndHardCap(t *testing.T) {
	r := newRecoveryRig(t, []string{"m"}, map[string]paneScript{"m": {pane: "x"}})
	r.lc.ModelRecovery = ModelRecoveryConfig{}
	base := r.lc.config()
	r.lc.SetConfig(recCfg{ConfigProvider: base, max: 2, deadline: time.Minute, attempt: 5 * time.Second, reject: time.Hour})
	require.Equal(t, ModelRecoveryConfig{MaxAttempts: 2, Deadline: time.Minute, AttemptTimeout: 5 * time.Second, RejectionTTL: time.Hour}, r.lc.modelRecoveryConfig())
	for _, n := range []int{4, 5, 99} {
		r.lc.SetConfig(recCfg{ConfigProvider: base, max: n})
		require.Equal(t, 3, r.lc.modelRecoveryConfig().MaxAttempts, "a provider may never raise the hard cap (%d)", n)
	}
	// An explicit programmatic override still wins over the provider.
	r.lc.ModelRecovery = ModelRecoveryConfig{MaxAttempts: 1}
	r.lc.SetConfig(recCfg{ConfigProvider: base, max: 3})
	require.Equal(t, 1, r.lc.modelRecoveryConfig().MaxAttempts)
}

func TestModelRecoveryHotReloadAppliesToNextLaunch(t *testing.T) {
	r := newRecoveryRig(t, []string{"m-a", "m-b", "m-c", "m-d", "m-e"}, map[string]paneScript{
		"m-a": rejected("m-a"), "m-b": rejected("m-b"), "m-c": rejected("m-c"), "m-d": rejected("m-d"), "m-e": rejected("m-e"),
	})
	base := r.lc.config()
	r.lc.ModelRecovery.MaxAttempts = 0

	r.lc.SetConfig(recCfg{ConfigProvider: base, max: 1})
	_, err := r.spawn("")
	var ex *ModelRecoveryExhaustedError
	require.True(t, errors.As(err, &ex), "got %v", err)
	require.Equal(t, 1, totalLaunches(r), "max_attempts=1: the first rejection is final")

	// A live reload (SetConfig, as the daemon's reload hook does) applies from the
	// very next launch, with no restart.
	r.lc.SetConfig(recCfg{ConfigProvider: base, max: 3})
	_, err = r.spawn("")
	require.True(t, errors.As(err, &ex), "got %v", err)
	require.Equal(t, 4, totalLaunches(r), "max_attempts=3 after reload: three more distinct launches (m-a is now suppressed by its own rejection evidence)")
}

func TestModelRecoveryRequestedFiveAttemptsStopAtThree(t *testing.T) {
	r := newRecoveryRig(t, []string{"m-a", "m-b", "m-c", "m-d", "m-e"}, map[string]paneScript{
		"m-a": rejected("m-a"), "m-b": rejected("m-b"), "m-c": rejected("m-c"), "m-d": rejected("m-d"), "m-e": rejected("m-e"),
	})
	r.lc.ModelRecovery.MaxAttempts = 0
	r.lc.SetConfig(recCfg{ConfigProvider: r.lc.config(), max: 5})
	_, err := r.spawn("")
	var ex *ModelRecoveryExhaustedError
	require.True(t, errors.As(err, &ex), "got %v", err)
	require.Len(t, r.launched(), 3, "never more than 3 distinct launches, whatever the config says")
}

func TestRecordedRejectionEventsReachThePersistenceHook(t *testing.T) {
	r, a := newRecordedRig(t, false, map[string]paneScript{"rec-model": rejected("rec-model")})
	var mu sync.Mutex
	var gotID string
	var got []store.Event
	r.lc.OnModelAttemptEvents = func(_ context.Context, id string, ev []store.Event) {
		mu.Lock()
		defer mu.Unlock()
		gotID, got = id, ev
	}
	err := r.lc.Restore(context.Background(), a)
	var rej *ModelRejectedError
	require.True(t, errors.As(err, &rej), "got %v", err)
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, "rec-agent", gotID)
	require.NotEmpty(t, got)
	for _, e := range got {
		require.Equal(t, "model_launch_attempt", e.Type)
	}
}

// totalLaunches counts every codex launch across all sessions of the rig.
func totalLaunches(r *recoveryRig) int {
	r.run.mu.Lock()
	defer r.run.mu.Unlock()
	n := 0
	for _, v := range r.run.launch {
		n += len(v)
	}
	return n
}
