package backendstore_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/backendstore"
	"github.com/stretchr/testify/require"
)

func TestStore_DefaultQuotasSeeded(t *testing.T) {
	s, err := backendstore.NewStore(t.TempDir())
	require.NoError(t, err)
	defer s.Close()

	quotas, err := s.ListQuotas()
	require.NoError(t, err)
	require.Len(t, quotas, 9)

	claude, err := s.GetQuota("claude", "session")
	require.NoError(t, err)
	require.Equal(t, "session", claude.Scope)
	require.Equal(t, backendstore.Window5HourRolling, claude.WindowType)
	require.Equal(t, 5*time.Hour, claude.WindowDuration)
	require.Equal(t, float64(500000), claude.QuotaLimit)

	codex, err := s.GetQuota("codex", "codex")
	require.NoError(t, err)
	require.Equal(t, "codex", codex.Scope)
	require.Equal(t, backendstore.Window5HourRolling, codex.WindowType)
	require.Equal(t, float64(500000), codex.QuotaLimit)

	gemini, err := s.GetQuota("antigravity", "gemini")
	require.NoError(t, err)
	require.Equal(t, "gemini", gemini.Scope)
	require.Equal(t, backendstore.Window5HourRolling, gemini.WindowType)

	api, err := s.GetQuota("cursor", "api")
	require.NoError(t, err)
	require.Equal(t, "api", api.Scope)
	require.Equal(t, backendstore.WindowMonthly, api.WindowType)
	require.Equal(t, float64(500), api.QuotaLimit)

	auto, err := s.GetQuota("cursor", "auto")
	require.NoError(t, err)
	require.Equal(t, "auto", auto.Scope)

	included, err := s.GetQuota("cursor", "included")
	require.NoError(t, err)
	require.Equal(t, "included", included.Scope)
}

func TestStore_ClaudeRolling5HourWindow(t *testing.T) {
	s, err := backendstore.NewStore(t.TempDir())
	require.NoError(t, err)
	defer s.Close()

	baseTime := time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC)

	// Initial headroom should be 1.0 (0% used)
	headroom, used, limit, limited, err := s.GetHeadroom("claude", baseTime)
	require.NoError(t, err)
	require.Equal(t, 1.0, headroom)
	require.Equal(t, 0.0, used)
	require.Equal(t, 500000.0, limit)
	require.False(t, limited)

	// Record 100,000 tokens at 10:00 (20% usage -> 80% headroom)
	err = s.RecordQuotaUsage("claude", 100000, "sonnet", baseTime)
	require.NoError(t, err)

	headroom, used, limit, limited, err = s.GetHeadroom("claude", baseTime.Add(30*time.Minute))
	require.NoError(t, err)
	require.Equal(t, 0.8, headroom)
	require.Equal(t, 100000.0, used)
	require.Equal(t, 500000.0, limit)
	require.False(t, limited)

	// Record another 300,000 tokens at 12:00 (total 400,000 tokens = 80% usage -> 20% headroom)
	err = s.RecordQuotaUsage("claude", 300000, "sonnet", baseTime.Add(2*time.Hour))
	require.NoError(t, err)

	headroom, used, _, _, err = s.GetHeadroom("claude", baseTime.Add(2*time.Hour))
	require.NoError(t, err)
	require.InDelta(t, 0.2, headroom, 0.001)
	require.Equal(t, 400000.0, used)

	// Advance time to 15:30 (5.5 hours after 10:00).
	// The 10:00 event (100,000 tokens) should have expired!
	// Only the 12:00 event (300,000 tokens) remains active (60% usage -> 40% headroom).
	headroom, used, _, _, err = s.GetHeadroom("claude", baseTime.Add(5*time.Hour+30*time.Minute))
	require.NoError(t, err)
	require.InDelta(t, 0.4, headroom, 0.001)
	require.Equal(t, 300000.0, used)

	// Advance time to 17:30 (5.5 hours after 12:00).
	// All events expired! Headroom should return to 1.0 (0 usage).
	headroom, used, _, _, err = s.GetHeadroom("claude", baseTime.Add(7*time.Hour+30*time.Minute))
	require.NoError(t, err)
	require.Equal(t, 1.0, headroom)
	require.Equal(t, 0.0, used)
}

func TestStore_AntigravityGeminiRollingWindow(t *testing.T) {
	s, err := backendstore.NewStore(t.TempDir())
	require.NoError(t, err)
	defer s.Close()

	base := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	// Record against a gemini model → gemini scope (5h rolling primary window).
	err = s.RecordQuotaUsage("antigravity", 800000, "gemini-3.1-pro-high", base)
	require.NoError(t, err)

	headroom, used, limit, limited, err := s.GetModelHeadroom("antigravity", "gemini-3.1-pro-high", base.Add(2*time.Hour))
	require.NoError(t, err)
	require.InDelta(t, 0.2, headroom, 0.001)
	require.Equal(t, 800000.0, used)
	require.Equal(t, 1000000.0, limit)
	require.False(t, limited)

	// Non-gemini models still see full headroom on their scope.
	hr, _, _, _, err := s.GetModelHeadroom("antigravity", "claude-sonnet-4-6", base.Add(2*time.Hour))
	require.NoError(t, err)
	require.Equal(t, 1.0, hr)
}

func TestStore_CursorMonthlyWindow(t *testing.T) {
	s, err := backendstore.NewStore(t.TempDir())
	require.NoError(t, err)
	defer s.Close()

	august := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

	// Record against an api-scoped model.
	err = s.RecordQuotaUsage("cursor", 400, "claude-opus-5-thinking-high", august)
	require.NoError(t, err)

	headroom, used, limit, _, err := s.GetModelHeadroom("cursor", "claude-opus-5-thinking-high", august.Add(2*time.Hour))
	require.NoError(t, err)
	require.InDelta(t, 0.2, headroom, 0.001)
	require.Equal(t, 400.0, used)
	require.Equal(t, 500.0, limit)

	// auto / included scopes unaffected
	hrAuto, _, _, _, err := s.GetModelHeadroom("cursor", "auto", august.Add(2*time.Hour))
	require.NoError(t, err)
	require.Equal(t, 1.0, hrAuto)

	hrInc, _, _, _, err := s.GetModelHeadroom("cursor", "composer-2.5-fast", august.Add(2*time.Hour))
	require.NoError(t, err)
	require.Equal(t, 1.0, hrInc)

	// Next month (September) → api resets
	september := time.Date(2026, 9, 1, 1, 0, 0, 0, time.UTC)
	headroom, used, _, _, err = s.GetModelHeadroom("cursor", "claude-opus-5-thinking-high", september)
	require.NoError(t, err)
	require.Equal(t, 1.0, headroom)
	require.Equal(t, 0.0, used)
}

func TestStore_LimitedUntilCooldown(t *testing.T) {
	s, err := backendstore.NewStore(t.TempDir())
	require.NoError(t, err)
	defer s.Close()

	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	cooldownUntil := now.Add(15 * time.Minute)

	// Set cooldown
	err = s.SetBackendLimited("claude", cooldownUntil)
	require.NoError(t, err)

	// During cooldown → headroom = 0.0, limited = true
	headroom, _, _, limited, err := s.GetHeadroom("claude", now.Add(5*time.Minute))
	require.NoError(t, err)
	require.Equal(t, 0.0, headroom)
	require.True(t, limited)

	// After cooldown → limited = false, headroom restored
	headroom, _, _, limited, err = s.GetHeadroom("claude", now.Add(20*time.Minute))
	require.NoError(t, err)
	require.Equal(t, 1.0, headroom)
	require.False(t, limited)
}

func TestStore_SetQuotaLimitAndReset(t *testing.T) {
	s, err := backendstore.NewStore(t.TempDir())
	require.NoError(t, err)
	defer s.Close()

	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	// Update quota limit for custom backend (default scope)
	err = s.SetQuotaLimit("custom", 200000, backendstore.Window5HourRolling, 2*time.Hour)
	require.NoError(t, err)

	q, err := s.GetQuota("custom", "default")
	require.NoError(t, err)
	require.Equal(t, "custom", q.BackendID)
	require.Equal(t, "default", q.Scope)
	require.Equal(t, 200000.0, q.QuotaLimit)
	require.Equal(t, 2*time.Hour, q.WindowDuration)

	// Record usage
	err = s.RecordQuotaUsage("custom", 100000, "m1", now)
	require.NoError(t, err)

	headroom, used, _, _, err := s.GetHeadroom("custom", now)
	require.NoError(t, err)
	require.Equal(t, 0.5, headroom)
	require.Equal(t, 100000.0, used)

	// Reset quota
	err = s.ResetQuota("custom")
	require.NoError(t, err)

	headroom, used, _, _, err = s.GetHeadroom("custom", now)
	require.NoError(t, err)
	require.Equal(t, 1.0, headroom)
	require.Equal(t, 0.0, used)
}

func TestBackendQuota_ScopedRoundTripMarshal(t *testing.T) {
	q := backendstore.BackendQuota{
		BackendID:      "cursor",
		Scope:          "auto",
		WindowType:     backendstore.WindowMonthly,
		WindowDuration: 30 * 24 * time.Hour,
		QuotaLimit:     500,
		UsedAmount:     45,
	}
	raw, err := json.Marshal(q)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"scope":"auto"`)

	var got backendstore.BackendQuota
	require.NoError(t, json.Unmarshal(raw, &got))
	require.Equal(t, "auto", got.Scope)
	require.Equal(t, "cursor", got.BackendID)
	require.Equal(t, 45.0, got.UsedAmount)
}

func TestStore_GetModelHeadroomPerScope(t *testing.T) {
	s, err := backendstore.NewStore(t.TempDir())
	require.NoError(t, err)
	defer s.Close()

	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

	// Exhaust api scope; leave auto and included full.
	require.NoError(t, s.SetQuota(backendstore.BackendQuota{
		BackendID:  "cursor",
		Scope:      "api",
		WindowType: backendstore.WindowMonthly,
		QuotaLimit: 100,
		UsedAmount: 100,
		LastReset:  now,
		UpdatedAt:  now,
	}))
	require.NoError(t, s.SetQuota(backendstore.BackendQuota{
		BackendID:  "cursor",
		Scope:      "auto",
		WindowType: backendstore.WindowMonthly,
		QuotaLimit: 100,
		UsedAmount: 45,
		LastReset:  now,
		UpdatedAt:  now,
	}))
	require.NoError(t, s.SetQuota(backendstore.BackendQuota{
		BackendID:  "cursor",
		Scope:      "included",
		WindowType: backendstore.WindowMonthly,
		QuotaLimit: 100,
		UsedAmount: 10,
		LastReset:  now,
		UpdatedAt:  now,
	}))

	hrAPI, _, _, limited, err := s.GetModelHeadroom("cursor", "claude-opus-5-thinking-high", now)
	require.NoError(t, err)
	require.Equal(t, 0.0, hrAPI)
	require.False(t, limited)

	hrAuto, used, limit, _, err := s.GetModelHeadroom("cursor", "auto", now)
	require.NoError(t, err)
	require.InDelta(t, 0.55, hrAuto, 0.001)
	require.Equal(t, 45.0, used)
	require.Equal(t, 100.0, limit)

	hrInc, _, _, _, err := s.GetModelHeadroom("cursor", "cursor-grok-4.6-high-fast", now)
	require.NoError(t, err)
	require.InDelta(t, 0.9, hrInc, 0.001)

	// Blank QuotaScope on a custom model → default scope (absent → full headroom).
	require.NoError(t, s.UpsertModel(backendstore.ModelEntry{
		BackendID: "cursor", ModelID: "custom-x", Tier: backendstore.Tier3, Enabled: true,
	}))
	hrDef, _, _, _, err := s.GetModelHeadroom("cursor", "custom-x", now)
	require.NoError(t, err)
	require.Equal(t, 1.0, hrDef)
}

func TestStore_GetHeadroomMinAcrossScopes(t *testing.T) {
	s, err := backendstore.NewStore(t.TempDir())
	require.NoError(t, err)
	defer s.Close()

	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

	require.NoError(t, s.SetQuota(backendstore.BackendQuota{
		BackendID: "cursor", Scope: "api", WindowType: backendstore.WindowMonthly,
		QuotaLimit: 100, UsedAmount: 100, LastReset: now, UpdatedAt: now,
	}))
	require.NoError(t, s.SetQuota(backendstore.BackendQuota{
		BackendID: "cursor", Scope: "auto", WindowType: backendstore.WindowMonthly,
		QuotaLimit: 100, UsedAmount: 45, LastReset: now, UpdatedAt: now,
	}))
	require.NoError(t, s.SetQuota(backendstore.BackendQuota{
		BackendID: "cursor", Scope: "included", WindowType: backendstore.WindowMonthly,
		QuotaLimit: 100, UsedAmount: 10, LastReset: now, UpdatedAt: now,
	}))

	// GetHeadroom is min across all scopes → api at 0.
	hr, used, limit, _, err := s.GetHeadroom("cursor", now)
	require.NoError(t, err)
	require.Equal(t, 0.0, hr)
	require.Equal(t, 100.0, used)
	require.Equal(t, 100.0, limit)
}

func TestStore_GetModelHeadroomMinAcrossWindows(t *testing.T) {
	s, err := backendstore.NewStore(t.TempDir())
	require.NoError(t, err)
	defer s.Close()

	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

	// 5h window half used; weekly nearly exhausted → min = weekly.
	require.NoError(t, s.SetQuota(backendstore.BackendQuota{
		BackendID: "antigravity", Scope: "gemini", WindowType: backendstore.Window5HourRolling,
		WindowDuration: 5 * time.Hour, QuotaLimit: 100,
		Events:    []backendstore.UsageEvent{{Timestamp: now, Amount: 50}},
		LastReset: now, UpdatedAt: now,
	}))
	require.NoError(t, s.SetQuota(backendstore.BackendQuota{
		BackendID: "antigravity", Scope: "gemini", WindowType: backendstore.WindowWeekly,
		WindowDuration: 7 * 24 * time.Hour, QuotaLimit: 100, UsedAmount: 90, LastReset: now, UpdatedAt: now,
	}))

	hr, used, limit, _, err := s.GetModelHeadroom("antigravity", "gemini-3.1-pro-high", now)
	require.NoError(t, err)
	require.InDelta(t, 0.1, hr, 0.001)
	require.Equal(t, 90.0, used)
	require.Equal(t, 100.0, limit)
}

func TestStore_MigrateScopeLessQuota(t *testing.T) {
	s, err := backendstore.NewStore(t.TempDir())
	require.NoError(t, err)
	defer s.Close()

	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

	// Blank Scope on UpsertQuota is rewritten to "default" under the scoped key.
	require.NoError(t, s.SetQuota(backendstore.BackendQuota{
		BackendID:  "legacy-be",
		Scope:      "",
		WindowType: backendstore.WindowDaily,
		QuotaLimit: 42,
		UsedAmount: 7,
		LastReset:  now,
		UpdatedAt:  now,
	}))

	q, err := s.GetQuota("legacy-be", "default")
	require.NoError(t, err)
	require.Equal(t, "default", q.Scope)
	require.Equal(t, 42.0, q.QuotaLimit)
	require.Equal(t, 7.0, q.UsedAmount)
}
