package backendusage

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/backendstore"
	"github.com/stretchr/testify/require"
)

func antigravityBackend() backendstore.Backend {
	return backendstore.Backend{ID: "antigravity", Installed: true, BinaryPath: "/synthetic/agy"}
}

func fixtureAvailableModels(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/antigravity-available-models.json")
	require.NoError(t, err)
	return raw
}

func TestAntigravityAdapterNotInstalled(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	a := AntigravityAdapter{Now: func() time.Time { return now }}
	got := a.Fetch(context.Background(), backendstore.Backend{ID: "antigravity", Installed: false})
	require.Equal(t, StatusNotInstalled, got.Status)
	require.Empty(t, got.Usage)
}

func TestAntigravityAdapterUnauthenticatedWhenTokenMissing(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	a := AntigravityAdapter{
		Now:       func() time.Time { return now },
		TokenPath: func() string { return "/nonexistent/antigravity-oauth-token" },
	}
	got := a.Fetch(context.Background(), antigravityBackend())
	require.Equal(t, StatusUnauthenticated, got.Status)
	require.Empty(t, got.Usage)
}

func TestAntigravityAdapterUnauthenticatedWhenEmptyToken(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	a := AntigravityAdapter{
		Now:       func() time.Time { return now },
		TokenPath: func() string { return "/synthetic/token" },
		ReadFile: func(string) ([]byte, error) {
			return []byte(`{"token":{},"auth_method":"consumer"}`), nil
		},
	}
	got := a.Fetch(context.Background(), antigravityBackend())
	require.Equal(t, StatusUnauthenticated, got.Status)
	require.Empty(t, got.Usage)
}

func TestAntigravityAdapterDualWindowFromFixture(t *testing.T) {
	modelsFixture := fixtureAvailableModels(t)
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)

	var tokenRefreshed bool
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenRefreshed = true
		require.Equal(t, http.MethodPost, r.Method)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"refreshed_test_token"}`)
	}))
	defer tokenSrv.Close()

	var modelsCalled bool
	modelsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		modelsCalled = true
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "Bearer refreshed_test_token", r.Header.Get("Authorization"))
		require.Equal(t, antigravityUserAgent, r.Header.Get("User-Agent"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(modelsFixture)
	}))
	defer modelsSrv.Close()

	a := AntigravityAdapter{
		Now:           func() time.Time { return now },
		Endpoint:      modelsSrv.URL,
		TokenEndpoint: tokenSrv.URL,
		TokenPath:     func() string { return "/synthetic/token" },
		ReadFile: func(string) ([]byte, error) {
			return []byte(`{
				"token": {
					"access_token": "expired_token",
					"refresh_token": "valid_refresh_token",
					"expiry": "2026-09-03T11:00:00Z"
				},
				"auth_method": "consumer"
			}`), nil
		},
	}

	got := a.Fetch(context.Background(), antigravityBackend())
	require.True(t, tokenRefreshed)
	require.True(t, modelsCalled)
	require.Equal(t, StatusOK, got.Status)
	require.NotNil(t, got.Account)
	require.Equal(t, "Free Tier", got.Account.Plan)
	require.Equal(t, "consumer", got.Account.LoginMethod)

	require.Len(t, got.Usage, 2)
	// Check gemini window
	gemini := got.Usage[0]
	require.Equal(t, "antigravity:gemini", gemini.ID)
	require.Equal(t, "gemini", gemini.Scope)
	require.Equal(t, "Gemini models", gemini.Label)
	require.Equal(t, []string{"gemini"}, gemini.ModelFamilies)
	require.Nil(t, gemini.Models)
	require.NotNil(t, gemini.UsedPercent)
	// remainingFraction in fixture was 0.6797259 => used % = 32.03
	require.InDelta(t, 32.03, *gemini.UsedPercent, 0.01)
	require.InDelta(t, 67.97, *gemini.RemainingPercent, 0.01)
	require.NotNil(t, gemini.ResetsAt)
	expectedReset, _ := time.Parse(time.RFC3339, "2026-09-03T13:22:40Z")
	require.Equal(t, expectedReset.UTC(), *gemini.ResetsAt)

	// Check non-gemini window
	nonGemini := got.Usage[1]
	require.Equal(t, "antigravity:non-gemini", nonGemini.ID)
	require.Equal(t, "non-gemini", nonGemini.Scope)
	require.Equal(t, "Non-Gemini models", nonGemini.Label)
	require.Nil(t, nonGemini.ModelFamilies)
	require.Nil(t, nonGemini.Models)
	require.NotNil(t, nonGemini.UsedPercent)
	// remainingFraction for claude/gpt was 1 => used % = 0
	require.InDelta(t, 0.0, *nonGemini.UsedPercent, 0.01)
	require.InDelta(t, 100.0, *nonGemini.RemainingPercent, 0.01)
	require.NotNil(t, nonGemini.ResetsAt)
	expectedNonGeminiReset, _ := time.Parse(time.RFC3339, "2026-09-03T16:30:36Z")
	require.Equal(t, expectedNonGeminiReset.UTC(), *nonGemini.ResetsAt)
}

func TestAntigravityAdapterFallbackWhenRPCFails(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	// When server returns 500, adapter falls back to null percents but remains StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	a := AntigravityAdapter{
		Now:       func() time.Time { return now },
		Endpoint:  srv.URL,
		TokenPath: func() string { return "/synthetic/token" },
		ReadFile: func(string) ([]byte, error) {
			return []byte(`{
				"token": {
					"access_token": "valid_token",
					"expiry": "2026-09-03T13:00:00Z"
				},
				"auth_method": "consumer"
			}`), nil
		},
	}

	got := a.Fetch(context.Background(), antigravityBackend())
	require.Equal(t, StatusOK, got.Status)
	require.Len(t, got.Usage, 2)
	require.Equal(t, "antigravity:gemini", got.Usage[0].ID)
	require.Nil(t, got.Usage[0].UsedPercent)
	require.Equal(t, "antigravity:non-gemini", got.Usage[1].ID)
	require.Nil(t, got.Usage[1].UsedPercent)
}

func TestAntigravityAdapterRateLimited(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"models": {
				"gemini-3.5-flash-low": {
					"displayName": "Gemini 3.5 Flash",
					"modelProvider": "MODEL_PROVIDER_GOOGLE",
					"apiProvider": "API_PROVIDER_GOOGLE_GEMINI",
					"quotaInfo": {
						"remainingFraction": 0,
						"resetTime": "2026-09-03T13:22:40Z"
					}
				}
			}
		}`)
	}))
	defer srv.Close()

	a := AntigravityAdapter{
		Now:       func() time.Time { return now },
		Endpoint:  srv.URL,
		TokenPath: func() string { return "/synthetic/token" },
		ReadFile: func(string) ([]byte, error) {
			return []byte(`{
				"token": {
					"access_token": "valid_token",
					"expiry": "2026-09-03T13:00:00Z"
				},
				"auth_method": "consumer"
			}`), nil
		},
	}

	got := a.Fetch(context.Background(), antigravityBackend())
	require.Equal(t, StatusRateLimited, got.Status)
	require.NotNil(t, got.Error)
	require.Equal(t, "rate_limited", got.Error.Code)
	require.Len(t, got.Usage, 2)
	require.Equal(t, float64(100), *got.Usage[0].UsedPercent)
	require.Equal(t, float64(0), *got.Usage[0].RemainingPercent)
	require.Equal(t, "reached", *got.Usage[0].LimitState)
}
