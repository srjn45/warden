package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/backendstore"
	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/store"
)

func TestHandleModelsRoutes(t *testing.T) {
	t.Run("unconfigured returns 503", func(t *testing.T) {
		srv := lifeServer(t, newFakeStore(), &fakeLife{})
		defer srv.Close()

		resp, err := http.Get(srv.URL + "/api/v1/models")
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)

		req, err := http.NewRequest(http.MethodPut, srv.URL+"/api/v1/models/claude/sonnet/tier", bytes.NewReader([]byte(`{"tier":"tier-1"}`)))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		resp2, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp2.Body.Close()
		require.Equal(t, http.StatusServiceUnavailable, resp2.StatusCode)
	})

	t.Run("list models and filter by tier", func(t *testing.T) {
		srv, _ := lifeServerBackends(t, newFakeStore(), &fakeLife{})
		defer srv.Close()

		resp, err := http.Get(srv.URL + "/api/v1/models")
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)

		var all []backendstore.ModelEntry
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&all))
		require.NotEmpty(t, all)

		// Filter by valid tier
		respTier, err := http.Get(srv.URL + "/api/v1/models?tier=tier-1")
		require.NoError(t, err)
		defer respTier.Body.Close()
		require.Equal(t, http.StatusOK, respTier.StatusCode)

		var tier1 []backendstore.ModelEntry
		require.NoError(t, json.NewDecoder(respTier.Body).Decode(&tier1))
		require.NotEmpty(t, tier1)
		for _, m := range tier1 {
			require.Equal(t, backendstore.Tier1, m.Tier)
		}

		// Filter by invalid tier -> 400
		respBad, err := http.Get(srv.URL + "/api/v1/models?tier=invalid-tier")
		require.NoError(t, err)
		defer respBad.Body.Close()
		require.Equal(t, http.StatusBadRequest, respBad.StatusCode)
	})

	t.Run("set model tier", func(t *testing.T) {
		srv, _ := lifeServerBackends(t, newFakeStore(), &fakeLife{})
		defer srv.Close()

		// Update claude/sonnet to tier-1
		body, _ := json.Marshal(map[string]string{"tier": "tier-1"})
		req, err := http.NewRequest(http.MethodPut, srv.URL+"/api/v1/models/claude/sonnet/tier", bytes.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)

		var updated backendstore.ModelEntry
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&updated))
		require.Equal(t, "claude", updated.BackendID)
		require.Equal(t, "sonnet", updated.ModelID)
		require.Equal(t, backendstore.Tier1, updated.Tier)

		// Invalid tier -> 400
		badBody, _ := json.Marshal(map[string]string{"tier": "tier-99"})
		reqBad, err := http.NewRequest(http.MethodPut, srv.URL+"/api/v1/models/claude/sonnet/tier", bytes.NewReader(badBody))
		require.NoError(t, err)
		reqBad.Header.Set("Content-Type", "application/json")
		respBad, err := http.DefaultClient.Do(reqBad)
		require.NoError(t, err)
		defer respBad.Body.Close()
		require.Equal(t, http.StatusBadRequest, respBad.StatusCode)

		// Non-existent model -> 404
		reqNotFound, err := http.NewRequest(http.MethodPut, srv.URL+"/api/v1/models/claude/non-existent-model/tier", bytes.NewReader(body))
		require.NoError(t, err)
		reqNotFound.Header.Set("Content-Type", "application/json")
		respNotFound, err := http.DefaultClient.Do(reqNotFound)
		require.NoError(t, err)
		defer respNotFound.Body.Close()
		require.Equal(t, http.StatusNotFound, respNotFound.StatusCode)
	})
}

func TestHandleRoleTiersRoutes(t *testing.T) {
	t.Run("unconfigured returns 503", func(t *testing.T) {
		srv := lifeServer(t, newFakeStore(), &fakeLife{})
		defer srv.Close()

		resp, err := http.Get(srv.URL + "/api/v1/roles/tiers")
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)

		req, err := http.NewRequest(http.MethodPut, srv.URL+"/api/v1/roles/tiers/implementation", bytes.NewReader([]byte(`{"tier":"tier-1"}`)))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		resp2, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp2.Body.Close()
		require.Equal(t, http.StatusServiceUnavailable, resp2.StatusCode)
	})

	t.Run("list and set role tiers", func(t *testing.T) {
		srv, _ := lifeServerBackends(t, newFakeStore(), &fakeLife{})
		defer srv.Close()

		// List role tiers
		resp, err := http.Get(srv.URL + "/api/v1/roles/tiers")
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)

		var mappings []backendstore.RoleTierMapping
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&mappings))
		require.NotEmpty(t, mappings)

		// Set role tier
		body, _ := json.Marshal(map[string]string{"tier": "tier-1"})
		req, err := http.NewRequest(http.MethodPut, srv.URL+"/api/v1/roles/tiers/implementation", bytes.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		respSet, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer respSet.Body.Close()
		require.Equal(t, http.StatusOK, respSet.StatusCode)

		var updated backendstore.RoleTierMapping
		require.NoError(t, json.NewDecoder(respSet.Body).Decode(&updated))
		require.Equal(t, "implementation", updated.RoleName)
		require.Equal(t, backendstore.Tier1, updated.DefaultTier)

		// Invalid tier -> 400
		badBody, _ := json.Marshal(map[string]string{"tier": "invalid"})
		reqBad, err := http.NewRequest(http.MethodPut, srv.URL+"/api/v1/roles/tiers/implementation", bytes.NewReader(badBody))
		require.NoError(t, err)
		reqBad.Header.Set("Content-Type", "application/json")
		respBad, err := http.DefaultClient.Do(reqBad)
		require.NoError(t, err)
		defer respBad.Body.Close()
		require.Equal(t, http.StatusBadRequest, respBad.StatusCode)
	})
}

func TestHandleHandoverSettingsRoutes(t *testing.T) {
	t.Run("unconfigured returns 503", func(t *testing.T) {
		srv := lifeServer(t, newFakeStore(), &fakeLife{})
		defer srv.Close()

		resp, err := http.Get(srv.URL + "/api/v1/handover/settings")
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)

		req, err := http.NewRequest(http.MethodPut, srv.URL+"/api/v1/handover/settings", bytes.NewReader([]byte(`{}`)))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		resp2, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp2.Body.Close()
		require.Equal(t, http.StatusServiceUnavailable, resp2.StatusCode)
	})

	t.Run("get and set handover settings", func(t *testing.T) {
		srv, _ := lifeServerBackends(t, newFakeStore(), &fakeLife{})
		defer srv.Close()

		// Get initial settings
		resp, err := http.Get(srv.URL + "/api/v1/handover/settings")
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)

		var current backendstore.HandoverSettings
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&current))
		require.True(t, current.Enabled)
		require.Equal(t, 90, current.ThresholdPercent)

		// Update settings
		newSettings := backendstore.HandoverSettings{
			Enabled:               false,
			ThresholdPercent:      85,
			RollingQuotaThreshold: 80,
			ContextFillThreshold:  85,
			CooldownPeriod:        30 * time.Minute,
		}
		body, _ := json.Marshal(newSettings)
		req, err := http.NewRequest(http.MethodPut, srv.URL+"/api/v1/handover/settings", bytes.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		respSet, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer respSet.Body.Close()
		require.Equal(t, http.StatusOK, respSet.StatusCode)

		var updated backendstore.HandoverSettings
		require.NoError(t, json.NewDecoder(respSet.Body).Decode(&updated))
		require.False(t, updated.Enabled)
		require.Equal(t, 85, updated.ThresholdPercent)
		require.Equal(t, 80, updated.RollingQuotaThreshold)
		require.Equal(t, 30*time.Minute, updated.CooldownPeriod)
	})
}

func TestHandleSwitchSessionRoute(t *testing.T) {
	fs := newFakeStore()
	fl := &fakeLife{}
	srv := lifeServer(t, fs, fl)
	defer srv.Close()

	// Seed an active session
	sess := &store.Session{
		ID:          "agent-switch-test",
		Backend:     "claude",
		Model:       "sonnet",
		TmuxSession: "agent-switch-test",
		Status:      store.StatusWorking,
	}
	require.NoError(t, fs.Insert(context.Background(), sess))

	t.Run("session not found", func(t *testing.T) {
		body, _ := json.Marshal(map[string]string{"backend": "antigravity"})
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/sessions/non-existent/switch", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusNotFound, resp.StatusCode)
	})

	t.Run("invalid tier", func(t *testing.T) {
		body, _ := json.Marshal(map[string]string{"tier": "tier-invalid"})
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/sessions/agent-switch-test/switch", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("successful switch", func(t *testing.T) {
		body, _ := json.Marshal(map[string]string{
			"backend": "antigravity",
			"model":   "gemini-3.1-pro",
			"reason":  "manual",
			"prompt":  "Continue testing",
		})
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/sessions/agent-switch-test/switch", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)

		var result lifecycle.SwapResult
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
		require.Equal(t, "antigravity", result.ToBackend)
		require.Equal(t, "gemini-3.1-pro", result.ToModel)
	})

	t.Run("lifecycle swap error maps to 400 or 500", func(t *testing.T) {
		fl.hotSwapErr = lifecycle.ErrNoSwapTarget
		body, _ := json.Marshal(map[string]string{})
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/sessions/agent-switch-test/switch", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
		fl.hotSwapErr = nil
	})
}
