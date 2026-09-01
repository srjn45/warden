package backendusage

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/backendstore"
	"github.com/stretchr/testify/require"
)

type scriptedRunner struct {
	status []byte
	about  []byte
	err    error
}

func (s scriptedRunner) Output(_ context.Context, _ string, args ...string) ([]byte, error) {
	if s.err != nil {
		return nil, s.err
	}
	joined := strings.Join(args, " ")
	switch {
	case strings.HasPrefix(joined, "status"):
		if s.status != nil {
			return s.status, nil
		}
		return []byte(`{"isAuthenticated":true}`), nil
	case strings.HasPrefix(joined, "about"):
		if s.about != nil {
			return s.about, nil
		}
		return []byte(`{"subscriptionTier":"Pro"}`), nil
	default:
		return nil, nil
	}
}

func cursorBackend() backendstore.Backend {
	return backendstore.Backend{ID: "cursor", Installed: true, BinaryPath: "/synthetic/cursor-agent"}
}

func fixturePeriodUsage(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/cursor-period-usage.json")
	require.NoError(t, err)
	return raw
}

func newCursorAdapter(t *testing.T, handler http.HandlerFunc, authToken string) CursorAdapter {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	now := time.Date(2026, 9, 1, 18, 31, 27, 0, time.UTC)
	return CursorAdapter{
		Runner:   scriptedRunner{},
		Now:      func() time.Time { return now },
		Endpoint: srv.URL,
		Doer:     srv.Client(),
		ReadFile: func(string) ([]byte, error) {
			return []byte(`{"accessToken":"` + authToken + `"}`), nil
		},
		AuthPath: func() string { return "/synthetic/cursor/auth.json" },
	}
}

func TestCursorAdapterThreeWindowsFromFixture(t *testing.T) {
	var gotPlan bool
	a := newCursorAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "1", r.Header.Get("Connect-Protocol-Version"))
		require.Equal(t, "Bearer synthetic-token", r.Header.Get("Authorization"))
		switch r.URL.Path {
		case cursorUsageRPC:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(fixturePeriodUsage(t))
		case cursorPlanRPC:
			gotPlan = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"planInfo":{"planName":"Pro"}}`))
		default:
			http.NotFound(w, r)
		}
	}, "synthetic-token")

	got := a.Fetch(context.Background(), cursorBackend())
	require.Equal(t, StatusRateLimited, got.Status)
	require.True(t, gotPlan)
	require.Equal(t, "Pro", got.Account.Plan)
	require.Equal(t, "cursor", got.Account.LoginMethod)
	require.Len(t, got.Usage, 3)

	byID := map[string]Limit{}
	for _, l := range got.Usage {
		byID[l.ID] = l
	}
	included := byID["cursor:included"]
	require.Equal(t, "included", included.Scope)
	require.Equal(t, "Included", included.Label)
	require.Equal(t, []string{"composer", "cursor-grok"}, included.ModelFamilies)
	require.Nil(t, included.Models)
	require.InDelta(t, 8.87, *included.UsedPercent, 0.001)
	require.InDelta(t, 91.13, *included.RemainingPercent, 0.001)
	require.Nil(t, included.LimitState)
	require.Equal(t, time.Date(2026, 10, 1, 18, 31, 27, 0, time.UTC), included.ResetsAt.UTC())

	auto := byID["cursor:auto"]
	require.Equal(t, "auto", auto.Scope)
	require.Equal(t, "Auto", auto.Label)
	require.Nil(t, auto.ModelFamilies)
	require.Equal(t, []string{"auto"}, auto.Models)
	require.InDelta(t, 4.09, *auto.UsedPercent, 0.001)
	require.Nil(t, auto.LimitState)

	api := byID["cursor:api"]
	require.Equal(t, "api", api.Scope)
	require.Equal(t, "API", api.Label)
	require.Equal(t, []string{"claude", "gpt", "gemini", "kimi", "glm"}, api.ModelFamilies)
	require.Nil(t, api.Models)
	require.Equal(t, float64(100), *api.UsedPercent)
	require.Equal(t, float64(0), *api.RemainingPercent)
	require.Equal(t, "reached", *api.LimitState)
	require.Equal(t, included.ResetsAt.UTC(), api.ResetsAt.UTC())
}

func TestCursorAdapterMissingPercentStaysNull(t *testing.T) {
	raw := fixturePeriodUsage(t)
	var body map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &body))
	var plan map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(body["planUsage"], &plan))
	delete(plan, "autoPercentUsed")
	patchedPlan, err := json.Marshal(plan)
	require.NoError(t, err)
	body["planUsage"] = patchedPlan
	patched, err := json.Marshal(body)
	require.NoError(t, err)

	a := newCursorAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != cursorUsageRPC {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(patched)
	}, "synthetic-token")

	got := a.Fetch(context.Background(), cursorBackend())
	require.Equal(t, StatusRateLimited, got.Status)
	var auto *Limit
	for i := range got.Usage {
		if got.Usage[i].ID == "cursor:auto" {
			auto = &got.Usage[i]
		}
	}
	require.NotNil(t, auto)
	require.Nil(t, auto.UsedPercent)
	require.Nil(t, auto.RemainingPercent)
	require.Nil(t, auto.LimitState)
}

func TestCursorAdapterHTTPFailureEmitsThreeNullRows(t *testing.T) {
	a := newCursorAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}, "synthetic-token")
	got := a.Fetch(context.Background(), cursorBackend())
	require.Equal(t, StatusOK, got.Status)
	require.Nil(t, got.Error)
	require.Len(t, got.Usage, 3)
	require.Equal(t, []string{"cursor:included", "cursor:auto", "cursor:api"}, []string{got.Usage[0].ID, got.Usage[1].ID, got.Usage[2].ID})
	for _, l := range got.Usage {
		require.NotEmpty(t, l.Scope)
		require.NotEmpty(t, l.Label)
		require.Nil(t, l.UsedPercent)
		require.Nil(t, l.ResetsAt)
	}
	require.Equal(t, []string{"composer", "cursor-grok"}, got.Usage[0].ModelFamilies)
	require.Equal(t, []string{"auto"}, got.Usage[1].Models)
	require.NotEmpty(t, got.Usage[2].ModelFamilies, "API selectors must not be empty")
}

func TestCursorAdapterBillingCycleEndNumberAndString(t *testing.T) {
	payloads := []string{
		`{"billingCycleEnd":"1790879487000","planUsage":{"totalPercentUsed":1,"autoPercentUsed":2,"apiPercentUsed":3}}`,
		`{"billingCycleEnd":1790879487000,"planUsage":{"totalPercentUsed":1,"autoPercentUsed":2,"apiPercentUsed":3}}`,
	}
	want := time.Date(2026, 10, 1, 18, 31, 27, 0, time.UTC)
	for _, payload := range payloads {
		payload := payload
		a := newCursorAdapter(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != cursorUsageRPC {
				http.NotFound(w, r)
				return
			}
			_, _ = io.WriteString(w, payload)
		}, "synthetic-token")
		got := a.Fetch(context.Background(), cursorBackend())
		require.Equal(t, StatusOK, got.Status)
		require.Equal(t, want, got.Usage[0].ResetsAt.UTC())
		require.Equal(t, want, got.Usage[2].ResetsAt.UTC())
	}
}

func TestCursorAdapterUnauthenticated(t *testing.T) {
	a := CursorAdapter{
		Runner: scriptedRunner{status: []byte(`{"isAuthenticated":false}`)},
		Now:    time.Now,
	}
	got := a.Fetch(context.Background(), cursorBackend())
	require.Equal(t, StatusUnauthenticated, got.Status)
	require.Empty(t, got.Usage)
}

func TestCursorAdapterNotInstalled(t *testing.T) {
	got := CursorAdapter{}.Fetch(context.Background(), backendstore.Backend{ID: "cursor", Installed: false})
	require.Equal(t, StatusNotInstalled, got.Status)
	require.Empty(t, got.Usage)
}

func TestCursorAdapterNoTokenStillEmitsThreeNullRows(t *testing.T) {
	var hits atomic.Int32
	a := newCursorAdapter(t, func(http.ResponseWriter, *http.Request) {
		hits.Add(1)
	}, "")
	a.ReadFile = func(string) ([]byte, error) { return []byte(`{}`), nil }
	got := a.Fetch(context.Background(), cursorBackend())
	require.Equal(t, StatusOK, got.Status)
	require.Len(t, got.Usage, 3)
	require.Zero(t, hits.Load())
	for _, l := range got.Usage {
		require.Nil(t, l.UsedPercent)
	}
}

func TestCursorAdapter401IsTransientWithoutPercents(t *testing.T) {
	a := newCursorAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}, "expired-token")
	got := a.Fetch(context.Background(), cursorBackend())
	require.Equal(t, StatusUnavailable, got.Status)
	require.Len(t, got.Usage, 3)
	for _, l := range got.Usage {
		require.Nil(t, l.UsedPercent)
	}
}

func TestCursorAdapterAuthPathLinuxDefault(t *testing.T) {
	dir := t.TempDir()
	auth := filepath.Join(dir, "cursor", "auth.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(auth), 0o755))
	require.NoError(t, os.WriteFile(auth, []byte(`{"accessToken":"from-file"}`), 0o600))
	t.Setenv("XDG_CONFIG_HOME", dir)

	var sawBearer string
	a := newCursorAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == cursorUsageRPC {
			sawBearer = r.Header.Get("Authorization")
			_, _ = w.Write([]byte(`{"planUsage":{"totalPercentUsed":10,"autoPercentUsed":20,"apiPercentUsed":30}}`))
			return
		}
		http.NotFound(w, r)
	}, "unused")
	a.ReadFile = nil
	a.AuthPath = nil
	got := a.Fetch(context.Background(), cursorBackend())
	require.Equal(t, StatusOK, got.Status)
	require.Equal(t, "Bearer from-file", sawBearer)
	require.Equal(t, float64(10), *got.Usage[0].UsedPercent)
}

func TestParseCursorPeriodUsageIgnoresAutoBucketModelsAndSpend(t *testing.T) {
	parsed, ok := parseCursorPeriodUsage(fixturePeriodUsage(t))
	require.True(t, ok)
	require.Len(t, parsed.limits, 3)
	require.True(t, parsed.rateLimited)
	require.Equal(t, []string{"composer", "cursor-grok"}, parsed.limits[0].ModelFamilies)
	require.Equal(t, []string{"auto"}, parsed.limits[1].Models)
	require.NotContains(t, parsed.limits[0].Models, "vega")
	require.NotContains(t, parsed.limits[0].Models, "grok-4.5")
}

func TestCursorSnakeCasePercents(t *testing.T) {
	raw := []byte(`{"billing_cycle_end":"1790879487000","plan_usage":{"total_percent_used":11,"auto_percent_used":22,"api_percent_used":33}}`)
	parsed, ok := parseCursorPeriodUsage(raw)
	require.True(t, ok)
	require.Equal(t, float64(11), *parsed.limits[0].UsedPercent)
	require.Equal(t, float64(22), *parsed.limits[1].UsedPercent)
	require.Equal(t, float64(33), *parsed.limits[2].UsedPercent)
	require.Equal(t, time.Date(2026, 10, 1, 18, 31, 27, 0, time.UTC), parsed.limits[0].ResetsAt.UTC())
}
