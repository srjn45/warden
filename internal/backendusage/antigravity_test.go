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

func fixtureQuotaSummary(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/antigravity-quota-summary.json")
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

func TestAntigravityAdapterFourWindowsFromSummary(t *testing.T) {
	summaryFixture := fixtureQuotaSummary(t)
	now := time.Date(2026, 9, 5, 20, 0, 0, 0, time.UTC)

	var tokenRefreshed bool
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenRefreshed = true
		require.Equal(t, http.MethodPost, r.Method)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"refreshed_test_token"}`)
	}))
	defer tokenSrv.Close()

	var summaryCalled bool
	summarySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		summaryCalled = true
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "Bearer refreshed_test_token", r.Header.Get("Authorization"))
		require.Equal(t, antigravityUserAgent, r.Header.Get("User-Agent"))
		require.Contains(t, r.URL.Path, "retrieveUserQuotaSummary")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(summaryFixture)
	}))
	defer summarySrv.Close()

	a := AntigravityAdapter{
		Now:           func() time.Time { return now },
		Endpoint:      summarySrv.URL,
		TokenEndpoint: tokenSrv.URL,
		TokenPath:     func() string { return "/synthetic/token" },
		ReadFile: func(string) ([]byte, error) {
			return []byte(`{
				"token": {
					"access_token": "expired_token",
					"refresh_token": "valid_refresh_token",
					"expiry": "2026-09-05T11:00:00Z"
				},
				"auth_method": "consumer"
			}`), nil
		},
	}

	got := a.Fetch(context.Background(), antigravityBackend())
	require.True(t, tokenRefreshed)
	require.True(t, summaryCalled)
	require.Equal(t, StatusRateLimited, got.Status)
	require.NotNil(t, got.Account)
	require.Equal(t, "Free Tier", got.Account.Plan)
	require.Equal(t, "consumer", got.Account.LoginMethod)

	require.Len(t, got.Usage, 4)

	gemini5h := got.Usage[0]
	require.Equal(t, "antigravity:gemini-5h", gemini5h.ID)
	require.Equal(t, "gemini", gemini5h.Scope)
	require.Equal(t, "Gemini 5-hour", gemini5h.Label)
	require.Equal(t, []string{"gemini"}, gemini5h.ModelFamilies)
	require.Equal(t, float64(100), *gemini5h.UsedPercent)
	require.Equal(t, float64(0), *gemini5h.RemainingPercent)
	require.Equal(t, antigravityFiveHourMinutes, *gemini5h.DurationMinutes)
	require.Equal(t, "reached", *gemini5h.LimitState)
	expected5h, _ := time.Parse(time.RFC3339, "2026-09-05T21:40:45Z")
	require.Equal(t, expected5h.UTC(), *gemini5h.ResetsAt)

	geminiWeekly := got.Usage[1]
	require.Equal(t, "antigravity:gemini-weekly", geminiWeekly.ID)
	require.Equal(t, "gemini", geminiWeekly.Scope)
	require.InDelta(t, 17.73, *geminiWeekly.UsedPercent, 0.01)
	require.InDelta(t, 82.27, *geminiWeekly.RemainingPercent, 0.01)
	require.Equal(t, antigravityWeeklyMinutes, *geminiWeekly.DurationMinutes)
	expectedWeekly, _ := time.Parse(time.RFC3339, "2026-09-10T19:03:34Z")
	require.Equal(t, expectedWeekly.UTC(), *geminiWeekly.ResetsAt)

	nonGemini5h := got.Usage[2]
	require.Equal(t, "antigravity:non-gemini-5h", nonGemini5h.ID)
	require.Equal(t, "non-gemini", nonGemini5h.Scope)
	require.InDelta(t, 0.0, *nonGemini5h.UsedPercent, 0.01)
	require.InDelta(t, 100.0, *nonGemini5h.RemainingPercent, 0.01)

	nonGeminiWeekly := got.Usage[3]
	require.Equal(t, "antigravity:non-gemini-weekly", nonGeminiWeekly.ID)
	require.InDelta(t, 0.0, *nonGeminiWeekly.UsedPercent, 0.01)
	require.InDelta(t, 100.0, *nonGeminiWeekly.RemainingPercent, 0.01)
}

func TestAntigravityAdapterFallbackWhenRPCFails(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
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
	require.Len(t, got.Usage, 4)
	require.Equal(t, "antigravity:gemini-5h", got.Usage[0].ID)
	require.Nil(t, got.Usage[0].UsedPercent)
	require.Equal(t, "antigravity:gemini-weekly", got.Usage[1].ID)
	require.Nil(t, got.Usage[1].UsedPercent)
	require.Equal(t, "antigravity:non-gemini-5h", got.Usage[2].ID)
	require.Nil(t, got.Usage[2].UsedPercent)
	require.Equal(t, "antigravity:non-gemini-weekly", got.Usage[3].ID)
	require.Nil(t, got.Usage[3].UsedPercent)
}
