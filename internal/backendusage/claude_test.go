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

func claudeBackend() backendstore.Backend {
	return backendstore.Backend{ID: "claude", Installed: true, BinaryPath: "/synthetic/claude"}
}

func fixtureClaudeUsage(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/claude-usage.json")
	require.NoError(t, err)
	return raw
}

func claudeCredsReader(token, sub string) func(string) ([]byte, error) {
	return func(string) ([]byte, error) {
		body := `{"claudeAiOauth":{"accessToken":"` + token + `","subscriptionType":"` + sub + `"}}`
		return []byte(body), nil
	}
}

func TestClaudeAdapterNotInstalled(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	a := ClaudeAdapter{Now: func() time.Time { return now }}
	got := a.Fetch(context.Background(), backendstore.Backend{ID: "claude", Installed: false})
	require.Equal(t, StatusNotInstalled, got.Status)
	require.Empty(t, got.Usage)
}

func TestClaudeAdapterUnauthenticatedWhenCredsMissing(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	a := ClaudeAdapter{
		Now:       func() time.Time { return now },
		CredsPath: func() string { return "/nonexistent/.credentials.json" },
	}
	got := a.Fetch(context.Background(), claudeBackend())
	require.Equal(t, StatusUnauthenticated, got.Status)
	require.Empty(t, got.Usage)
}

func TestClaudeAdapterUnauthenticatedWhenEmptyToken(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	a := ClaudeAdapter{
		Now:       func() time.Time { return now },
		CredsPath: func() string { return "/synthetic/creds" },
		ReadFile: func(string) ([]byte, error) {
			return []byte(`{"claudeAiOauth":{"accessToken":"","subscriptionType":"pro"}}`), nil
		},
	}
	got := a.Fetch(context.Background(), claudeBackend())
	require.Equal(t, StatusUnauthenticated, got.Status)
	require.Empty(t, got.Usage)
}

func TestClaudeAdapterSessionWindowFromFixture(t *testing.T) {
	fixture := fixtureClaudeUsage(t)
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)

	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "Bearer valid_token", r.Header.Get("Authorization"))
		require.Equal(t, claudeOAuthBeta, r.Header.Get("anthropic-beta"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture)
	}))
	defer srv.Close()

	a := ClaudeAdapter{
		Now:       func() time.Time { return now },
		Endpoint:  srv.URL,
		CredsPath: func() string { return "/synthetic/creds" },
		ReadFile:  claudeCredsReader("valid_token", "pro"),
	}

	got := a.Fetch(context.Background(), claudeBackend())
	require.True(t, called)
	require.Equal(t, StatusOK, got.Status)
	require.NotNil(t, got.Account)
	require.Equal(t, "Pro", got.Account.Plan)
	require.Equal(t, "claude.ai", got.Account.LoginMethod)

	require.Len(t, got.Usage, 1)
	session := got.Usage[0]
	require.Equal(t, "claude:session", session.ID)
	require.Equal(t, "session", session.Scope)
	require.Equal(t, "Session (5-hour)", session.Label)
	require.Nil(t, session.ModelFamilies)
	require.Nil(t, session.Models)
	require.NotNil(t, session.UsedPercent)
	require.InDelta(t, 8.0, *session.UsedPercent, 0.001)
	require.NotNil(t, session.RemainingPercent)
	require.InDelta(t, 92.0, *session.RemainingPercent, 0.001)
	require.NotNil(t, session.DurationMinutes)
	require.Equal(t, claudeSessionWindowMinutes, *session.DurationMinutes)
	require.NotNil(t, session.ResetsAt)
	expectedReset, _ := time.Parse(time.RFC3339, "2026-09-03T17:00:00.342241+00:00")
	require.Equal(t, expectedReset.UTC(), *session.ResetsAt)
	require.Nil(t, session.LimitState)
}

func TestClaudeAdapterFallbackWhenEndpointFails(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	a := ClaudeAdapter{
		Now:       func() time.Time { return now },
		Endpoint:  srv.URL,
		CredsPath: func() string { return "/synthetic/creds" },
		ReadFile:  claudeCredsReader("stale_token", "max"),
	}

	got := a.Fetch(context.Background(), claudeBackend())
	require.Equal(t, StatusOK, got.Status)
	require.Equal(t, "Max", got.Account.Plan)
	require.Len(t, got.Usage, 1)
	require.Equal(t, "claude:session", got.Usage[0].ID)
	require.Nil(t, got.Usage[0].UsedPercent)
	require.Nil(t, got.Usage[0].RemainingPercent)
	require.Nil(t, got.Usage[0].ResetsAt)
}

func TestClaudeAdapterRateLimited(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"five_hour":{"utilization":100,"resets_at":"2026-09-03T17:00:00Z"}}`)
	}))
	defer srv.Close()

	a := ClaudeAdapter{
		Now:       func() time.Time { return now },
		Endpoint:  srv.URL,
		CredsPath: func() string { return "/synthetic/creds" },
		ReadFile:  claudeCredsReader("valid_token", "pro"),
	}

	got := a.Fetch(context.Background(), claudeBackend())
	require.Equal(t, StatusRateLimited, got.Status)
	require.NotNil(t, got.Error)
	require.Equal(t, "rate_limited", got.Error.Code)
	require.Len(t, got.Usage, 1)
	require.Equal(t, float64(100), *got.Usage[0].UsedPercent)
	require.Equal(t, float64(0), *got.Usage[0].RemainingPercent)
	require.NotNil(t, got.Usage[0].LimitState)
	require.Equal(t, "reached", *got.Usage[0].LimitState)
}
