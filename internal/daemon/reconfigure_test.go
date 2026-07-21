package daemon

import (
	"testing"
	"time"

	"github.com/srjn45/warden/internal/config"
	"github.com/srjn45/warden/internal/poller"
	"github.com/stretchr/testify/require"
)

// TestApplyConfigFansOutLive proves the hot-reload fan-out pushes a reloaded
// config into the subsystems the Server owns (poller auto-approve + context
// guard, api_docs, scheduler route gate) AND runs the registered reload hooks
// (lifecycle / notifier / autopilot live in daemon.go), updating the baseline.
func TestApplyConfigFansOutLive(t *testing.T) {
	pl := poller.New(nil, time.Minute)
	srv := &Server{poller: pl}

	// Seed a baseline so the restart-only diff has something to compare against.
	srv.SetBaselineConfig(config.Config{Addr: "127.0.0.1:8765"})

	var hookGot config.Config
	hookRan := 0
	srv.AddReloadHook(func(c config.Config) { hookRan++; hookGot = c })

	newCfg := config.Config{
		Addr:             "127.0.0.1:9000", // restart-only: reported, not applied
		ApiDocs:          true,
		SchedulerEnabled: true,
	}
	newCfg.AutoApprove.Enabled = true
	newCfg.Tokens.Guard = true
	newCfg.Tokens.Warn = 123000
	newCfg.Tokens.Critical = 456000
	newCfg.Tokens.WarnAlert = true
	newCfg.Tokens.AutoCompact = true
	newCfg.Tokens.CompactResumePrompt = "go on"

	srv.ApplyConfig(newCfg)

	// Poller: auto-approve + context guard applied live.
	require.True(t, pl.AutoApprovePolicySnapshot().Enabled)
	require.True(t, pl.TokenGuard)
	require.Equal(t, 123000, pl.TokenWarn)
	require.Equal(t, 456000, pl.TokenCrit)
	require.True(t, pl.WarnAlert)
	require.True(t, pl.AutoCompact)
	require.Equal(t, "go on", pl.CompactResumePrompt)

	// Server-owned gates applied live.
	require.True(t, srv.apiDocs)
	require.True(t, srv.scheduler)

	// Reload hook ran with the new config; baseline advanced.
	require.Equal(t, 1, hookRan)
	require.Equal(t, "127.0.0.1:9000", hookGot.Addr)
	srv.reloadMu.Lock()
	require.Equal(t, newCfg, srv.appliedConfig)
	srv.reloadMu.Unlock()
}

// TestApplyConfigNilPollerSafe proves the fan-out tolerates an unconfigured
// poller (bare Server literal) — it still applies its own gates and runs hooks.
func TestApplyConfigNilPollerSafe(t *testing.T) {
	srv := &Server{}
	ran := false
	srv.AddReloadHook(func(config.Config) { ran = true })
	require.NotPanics(t, func() { srv.ApplyConfig(config.Config{ApiDocs: true}) })
	require.True(t, srv.apiDocs)
	require.True(t, ran)
}
