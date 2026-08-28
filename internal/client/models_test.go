package client

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/backendstore"
	"github.com/srjn45/warden/internal/lifecycle"
)

func TestClientListModels(t *testing.T) {
	var c capture
	ts := jsonServer(t, &c, 0, `[{"backend_id":"claude","model_id":"claude-3-7-sonnet-20250219","tier":"tier-2","display_name":"Claude 3.7 Sonnet","enabled":true}]`)
	cl := New(ts.URL)

	models, err := cl.ListModels(context.Background(), "")
	require.NoError(t, err)
	require.Equal(t, http.MethodGet, c.method)
	require.Equal(t, "/api/v1/models", c.path)
	require.Len(t, models, 1)
	require.Equal(t, "claude-3-7-sonnet-20250219", models[0].ModelID)

	_, err = cl.ListModels(context.Background(), "tier-1")
	require.NoError(t, err)
	require.Equal(t, "/api/v1/models", c.path)
	require.Equal(t, "tier=tier-1", c.rawQ)
}

func TestClientSetModelTier(t *testing.T) {
	var c capture
	ts := jsonServer(t, &c, 0, `{"backend_id":"claude","model_id":"claude-3-7-sonnet-20250219","tier":"tier-1","display_name":"Claude 3.7 Sonnet","enabled":true}`)
	cl := New(ts.URL)

	m, err := cl.SetModelTier(context.Background(), "claude", "claude-3-7-sonnet-20250219", "tier-1")
	require.NoError(t, err)
	require.Equal(t, http.MethodPut, c.method)
	require.Equal(t, "/api/v1/models/claude/claude-3-7-sonnet-20250219/tier", c.path)
	require.Equal(t, "tier-1", c.body["tier"])
	require.Equal(t, backendstore.Tier1, m.Tier)
}

func TestClientListRoleTiers(t *testing.T) {
	var c capture
	ts := jsonServer(t, &c, 0, `[{"role_name":"implementation","default_tier":"tier-2"}]`)
	cl := New(ts.URL)

	mappings, err := cl.ListRoleTiers(context.Background())
	require.NoError(t, err)
	require.Equal(t, http.MethodGet, c.method)
	require.Equal(t, "/api/v1/roles/tiers", c.path)
	require.Len(t, mappings, 1)
	require.Equal(t, "implementation", mappings[0].RoleName)
}

func TestClientSetRoleTier(t *testing.T) {
	var c capture
	ts := jsonServer(t, &c, 0, `{"role_name":"implementation","default_tier":"tier-1"}`)
	cl := New(ts.URL)

	mapping, err := cl.SetRoleTier(context.Background(), "implementation", "tier-1")
	require.NoError(t, err)
	require.Equal(t, http.MethodPut, c.method)
	require.Equal(t, "/api/v1/roles/tiers/implementation", c.path)
	require.Equal(t, "tier-1", c.body["tier"])
	require.Equal(t, backendstore.Tier1, mapping.DefaultTier)
}

func TestClientSwitchSession(t *testing.T) {
	var c capture
	ts := jsonServer(t, &c, 0, `{"from_backend":"claude","to_backend":"antigravity","to_model":"gemini-3.1-pro","reason":"manual"}`)
	cl := New(ts.URL)

	res, err := cl.SwitchSession(context.Background(), "agent-123", SwitchSessionParams{
		Backend: "antigravity",
		Model:   "gemini-3.1-pro",
		Reason:  "manual",
	})
	require.NoError(t, err)
	require.Equal(t, http.MethodPost, c.method)
	require.Equal(t, "/api/v1/sessions/agent-123/switch", c.path)
	require.Equal(t, "antigravity", c.body["backend"])
	require.Equal(t, "antigravity", res.ToBackend)
	require.Equal(t, lifecycle.SwapReasonManual, res.Reason)
}

func TestClientGetHandoverSettings(t *testing.T) {
	var c capture
	ts := jsonServer(t, &c, 0, `{"enabled":true,"threshold_percent":90,"rolling_quota_threshold":90,"context_fill_threshold":90,"cooldown_period":900000000000}`)
	cl := New(ts.URL)

	settings, err := cl.GetHandoverSettings(context.Background())
	require.NoError(t, err)
	require.Equal(t, http.MethodGet, c.method)
	require.Equal(t, "/api/v1/handover/settings", c.path)
	require.True(t, settings.Enabled)
	require.Equal(t, 90, settings.ThresholdPercent)
	require.Equal(t, 15*time.Minute, settings.CooldownPeriod)
}

func TestClientSetHandoverSettings(t *testing.T) {
	var c capture
	ts := jsonServer(t, &c, 0, `{"enabled":false,"threshold_percent":85,"rolling_quota_threshold":80,"context_fill_threshold":85,"cooldown_period":1800000000000}`)
	cl := New(ts.URL)

	settings, err := cl.SetHandoverSettings(context.Background(), backendstore.HandoverSettings{
		Enabled:               false,
		ThresholdPercent:      85,
		RollingQuotaThreshold: 80,
		ContextFillThreshold:  85,
		CooldownPeriod:        30 * time.Minute,
	})
	require.NoError(t, err)
	require.Equal(t, http.MethodPut, c.method)
	require.Equal(t, "/api/v1/handover/settings", c.path)
	require.False(t, settings.Enabled)
	require.Equal(t, 85, settings.ThresholdPercent)
	require.Equal(t, 30*time.Minute, settings.CooldownPeriod)
}
