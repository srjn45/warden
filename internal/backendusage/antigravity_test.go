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

func TestAntigravityAdapterTwoBucketsFromSummary(t *testing.T) {
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

	require.Len(t, got.Usage, 2)

	// Gemini: weekly (0.82) still has headroom, so the bucket reports its
	// 5-hour session limit — which is exhausted (remainingFraction 0).
	gemini := got.Usage[0]
	require.Equal(t, "antigravity:gemini", gemini.ID)
	require.Equal(t, "gemini", gemini.Scope)
	require.Equal(t, "Gemini", gemini.Label)
	require.Equal(t, []string{"gemini"}, gemini.ModelFamilies)
	require.Equal(t, float64(100), *gemini.UsedPercent)
	require.Equal(t, float64(0), *gemini.RemainingPercent)
	require.Equal(t, antigravityFiveHourMinutes, *gemini.DurationMinutes)
	require.Equal(t, "reached", *gemini.LimitState)
	expected5h, _ := time.Parse(time.RFC3339, "2026-09-05T21:40:45Z")
	require.Equal(t, expected5h.UTC(), *gemini.ResetsAt)

	// Non-Gemini: weekly (1.0) has headroom, reports the 5-hour session limit
	// which is untouched (remainingFraction 1).
	nonGemini := got.Usage[1]
	require.Equal(t, "antigravity:non-gemini", nonGemini.ID)
	require.Equal(t, "non-gemini", nonGemini.Scope)
	require.Equal(t, "Non-Gemini", nonGemini.Label)
	require.InDelta(t, 0.0, *nonGemini.UsedPercent, 0.01)
	require.InDelta(t, 100.0, *nonGemini.RemainingPercent, 0.01)
	require.Equal(t, antigravityFiveHourMinutes, *nonGemini.DurationMinutes)
	require.Nil(t, nonGemini.LimitState)
	expectedNG5h, _ := time.Parse(time.RFC3339, "2026-09-06T01:04:38Z")
	require.Equal(t, expectedNG5h.UTC(), *nonGemini.ResetsAt)
}

// TestAntigravityWeeklyExhaustionOverride asserts that once a pool's weekly
// limit is exhausted (remainingFraction <= 0), its bucket flips to the weekly
// limit fully consumed — 100% used, "reached", weekly duration and weekly reset
// — regardless of the 5-hour session bucket still having headroom.
func TestAntigravityWeeklyExhaustionOverride(t *testing.T) {
	body := []byte(`{
		"groups": [
			{
				"displayName": "Gemini Models",
				"buckets": [
					{"bucketId": "gemini-weekly", "window": "weekly", "resetTime": "2026-09-12T08:00:00Z", "remainingFraction": 0},
					{"bucketId": "gemini-5h", "window": "5h", "resetTime": "2026-09-06T01:00:00Z", "remainingFraction": 0.5}
				]
			},
			{
				"displayName": "Claude and GPT models",
				"buckets": [
					{"bucketId": "3p-weekly", "window": "weekly", "resetTime": "2026-09-12T09:00:00Z", "remainingFraction": 0.9},
					{"bucketId": "3p-5h", "window": "5h", "resetTime": "2026-09-06T02:00:00Z", "remainingFraction": 0.25}
				]
			}
		]
	}`)

	limits, ok := parseAntigravityQuotaSummary(body)
	require.True(t, ok)
	require.Len(t, limits, 2)

	// Gemini weekly exhausted → weekly override (5-hour headroom ignored).
	gemini := limits[0]
	require.Equal(t, "antigravity:gemini", gemini.ID)
	require.Equal(t, float64(100), *gemini.UsedPercent)
	require.Equal(t, float64(0), *gemini.RemainingPercent)
	require.Equal(t, "reached", *gemini.LimitState)
	require.Equal(t, antigravityWeeklyMinutes, *gemini.DurationMinutes)
	expectedWeekly, _ := time.Parse(time.RFC3339, "2026-09-12T08:00:00Z")
	require.Equal(t, expectedWeekly.UTC(), *gemini.ResetsAt)

	// Non-Gemini weekly (0.9) has headroom → reports the 5-hour session limit.
	nonGemini := limits[1]
	require.Equal(t, "antigravity:non-gemini", nonGemini.ID)
	require.InDelta(t, 75.0, *nonGemini.UsedPercent, 0.01)
	require.InDelta(t, 25.0, *nonGemini.RemainingPercent, 0.01)
	require.Nil(t, nonGemini.LimitState)
	require.Equal(t, antigravityFiveHourMinutes, *nonGemini.DurationMinutes)
	expectedNG5h, _ := time.Parse(time.RFC3339, "2026-09-06T02:00:00Z")
	require.Equal(t, expectedNG5h.UTC(), *nonGemini.ResetsAt)
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
	require.Len(t, got.Usage, 2)
	require.Equal(t, "antigravity:gemini", got.Usage[0].ID)
	require.Nil(t, got.Usage[0].UsedPercent)
	require.Equal(t, "antigravity:non-gemini", got.Usage[1].ID)
	require.Nil(t, got.Usage[1].UsedPercent)
}
