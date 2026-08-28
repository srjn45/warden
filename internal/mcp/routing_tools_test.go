package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/backendstore"
	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/store"
)

func TestListModelsTool(t *testing.T) {
	var query string
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/models", r.URL.Path)
		query = r.URL.RawQuery
		models := []backendstore.ModelEntry{
			{
				BackendID:   "claude",
				ModelID:     "claude-3-7-sonnet-20250219",
				Tier:        backendstore.Tier2,
				DisplayName: "Claude 3.7 Sonnet",
				Enabled:     true,
			},
		}
		_ = json.NewEncoder(w).Encode(models)
	}))
	defer daemon.Close()

	session := connectTo(t, daemon.URL)
	ctx := context.Background()

	// Without filter
	res, err := session.CallTool(ctx, &mcpsdk.CallToolParams{
		Name: "list_models",
	})
	require.NoError(t, err)
	require.False(t, res.IsError, textOf(res))
	require.Contains(t, textOf(res), "claude-3-7-sonnet-20250219")
	require.Empty(t, query)

	// With tier filter
	res2, err := session.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "list_models",
		Arguments: map[string]any{"tier": "tier-2"},
	})
	require.NoError(t, err)
	require.False(t, res2.IsError, textOf(res2))
	require.Equal(t, "tier=tier-2", query)
}

func TestSetModelTierTool(t *testing.T) {
	var hit, body string
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = r.Method + " " + r.URL.Path
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		m := backendstore.ModelEntry{
			BackendID: "claude",
			ModelID:   "claude-3-7-sonnet-20250219",
			Tier:      backendstore.Tier1,
		}
		_ = json.NewEncoder(w).Encode(m)
	}))
	defer daemon.Close()

	session := connectTo(t, daemon.URL)
	ctx := context.Background()

	res, err := session.CallTool(ctx, &mcpsdk.CallToolParams{
		Name: "set_model_tier",
		Arguments: map[string]any{
			"backend": "claude",
			"model":   "claude-3-7-sonnet-20250219",
			"tier":    "tier-1",
		},
	})
	require.NoError(t, err)
	require.False(t, res.IsError, textOf(res))
	require.Equal(t, "PUT /api/v1/models/claude/claude-3-7-sonnet-20250219/tier", hit)
	require.Contains(t, body, `"tier":"tier-1"`)
	require.Contains(t, textOf(res), `"tier": "tier-1"`)
}

func TestListRoleTiersTool(t *testing.T) {
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/roles/tiers", r.URL.Path)
		mappings := []backendstore.RoleTierMapping{
			{RoleName: "implementation", DefaultTier: backendstore.Tier2},
			{RoleName: "architecture", DefaultTier: backendstore.Tier1},
		}
		_ = json.NewEncoder(w).Encode(mappings)
	}))
	defer daemon.Close()

	session := connectTo(t, daemon.URL)
	ctx := context.Background()

	res, err := session.CallTool(ctx, &mcpsdk.CallToolParams{
		Name: "list_role_tiers",
	})
	require.NoError(t, err)
	require.False(t, res.IsError, textOf(res))
	require.Contains(t, textOf(res), "implementation")
	require.Contains(t, textOf(res), "architecture")
}

func TestSetRoleTierTool(t *testing.T) {
	var hit, body string
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = r.Method + " " + r.URL.Path
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		m := backendstore.RoleTierMapping{
			RoleName:    "implementation",
			DefaultTier: backendstore.Tier1,
		}
		_ = json.NewEncoder(w).Encode(m)
	}))
	defer daemon.Close()

	session := connectTo(t, daemon.URL)
	ctx := context.Background()

	res, err := session.CallTool(ctx, &mcpsdk.CallToolParams{
		Name: "set_role_tier",
		Arguments: map[string]any{
			"role": "implementation",
			"tier": "tier-1",
		},
	})
	require.NoError(t, err)
	require.False(t, res.IsError, textOf(res))
	require.Equal(t, "PUT /api/v1/roles/tiers/implementation", hit)
	require.Contains(t, body, `"tier":"tier-1"`)
	require.Contains(t, textOf(res), "implementation")
}

func TestSwitchAgentTool(t *testing.T) {
	var hit, body string
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = r.Method + " " + r.URL.Path
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		res := lifecycle.SwapResult{
			Session:     &store.Session{ID: "agent-123"},
			FromBackend: "claude",
			ToBackend:   "antigravity",
			ToModel:     "gemini-3.1-pro",
			Reason:      lifecycle.SwapReasonManual,
		}
		_ = json.NewEncoder(w).Encode(res)
	}))
	defer daemon.Close()

	session := connectTo(t, daemon.URL)
	ctx := context.Background()

	res, err := session.CallTool(ctx, &mcpsdk.CallToolParams{
		Name: "switch_agent",
		Arguments: map[string]any{
			"ticket":  "agent-123",
			"backend": "antigravity",
			"model":   "gemini-3.1-pro",
			"reason":  "manual",
			"prompt":  "Continue task",
		},
	})
	require.NoError(t, err)
	require.False(t, res.IsError, textOf(res))
	require.Equal(t, "POST /api/v1/sessions/agent-123/switch", hit)
	require.Contains(t, body, `"backend":"antigravity"`)
	require.Contains(t, body, `"model":"gemini-3.1-pro"`)
	require.Contains(t, textOf(res), "antigravity")
}

func TestGetHandoverSettingsTool(t *testing.T) {
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/handover/settings", r.URL.Path)
		settings := backendstore.HandoverSettings{
			Enabled:               true,
			ThresholdPercent:      90,
			RollingQuotaThreshold: 90,
			ContextFillThreshold:  90,
			CooldownPeriod:        15 * time.Minute,
		}
		_ = json.NewEncoder(w).Encode(settings)
	}))
	defer daemon.Close()

	session := connectTo(t, daemon.URL)
	ctx := context.Background()

	res, err := session.CallTool(ctx, &mcpsdk.CallToolParams{
		Name: "get_handover_settings",
	})
	require.NoError(t, err)
	require.False(t, res.IsError, textOf(res))
	require.Contains(t, textOf(res), `"enabled": true`)
	require.Contains(t, textOf(res), `"threshold_percent": 90`)
}

func TestSetHandoverSettingsTool(t *testing.T) {
	var putBody string
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			require.Equal(t, "/api/v1/handover/settings", r.URL.Path)
			settings := backendstore.HandoverSettings{
				Enabled:               true,
				ThresholdPercent:      90,
				RollingQuotaThreshold: 90,
				ContextFillThreshold:  90,
				CooldownPeriod:        15 * time.Minute,
			}
			_ = json.NewEncoder(w).Encode(settings)
			return
		}
		if r.Method == http.MethodPut {
			require.Equal(t, "/api/v1/handover/settings", r.URL.Path)
			b, _ := io.ReadAll(r.Body)
			putBody = string(b)
			var updated backendstore.HandoverSettings
			_ = json.Unmarshal(b, &updated)
			_ = json.NewEncoder(w).Encode(updated)
			return
		}
		http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
	}))
	defer daemon.Close()

	session := connectTo(t, daemon.URL)
	ctx := context.Background()

	res, err := session.CallTool(ctx, &mcpsdk.CallToolParams{
		Name: "set_handover_settings",
		Arguments: map[string]any{
			"enabled":           false,
			"threshold_percent": 85,
			"cooldown_period":   "30m",
		},
	})
	require.NoError(t, err)
	require.False(t, res.IsError, textOf(res))
	require.Contains(t, putBody, `"enabled":false`)
	require.Contains(t, putBody, `"threshold_percent":85`)
	require.Contains(t, textOf(res), `"enabled": false`)
}
