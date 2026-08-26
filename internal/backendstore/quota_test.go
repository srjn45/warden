package backendstore_test

import (
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
	require.Len(t, quotas, 4)

	claude, err := s.GetQuota("claude")
	require.NoError(t, err)
	require.Equal(t, backendstore.Window5HourRolling, claude.WindowType)
	require.Equal(t, 5*time.Hour, claude.WindowDuration)
	require.Equal(t, float64(500000), claude.QuotaLimit)

	antigravity, err := s.GetQuota("antigravity")
	require.NoError(t, err)
	require.Equal(t, backendstore.WindowDaily, antigravity.WindowType)
	require.Equal(t, 24*time.Hour, antigravity.WindowDuration)
	require.Equal(t, float64(1000000), antigravity.QuotaLimit)

	cursor, err := s.GetQuota("cursor")
	require.NoError(t, err)
	require.Equal(t, backendstore.WindowMonthly, cursor.WindowType)
	require.Equal(t, float64(500), cursor.QuotaLimit)

	codex, err := s.GetQuota("codex")
	require.NoError(t, err)
	require.Equal(t, backendstore.Window5HourRolling, codex.WindowType)
	require.Equal(t, float64(500000), codex.QuotaLimit)
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
	err = s.RecordQuotaUsage("claude", 100000, "claude-3-7-sonnet", baseTime)
	require.NoError(t, err)

	headroom, used, limit, limited, err = s.GetHeadroom("claude", baseTime.Add(30*time.Minute))
	require.NoError(t, err)
	require.Equal(t, 0.8, headroom)
	require.Equal(t, 100000.0, used)
	require.Equal(t, 500000.0, limit)
	require.False(t, limited)

	// Record another 300,000 tokens at 12:00 (total 400,000 tokens = 80% usage -> 20% headroom)
	err = s.RecordQuotaUsage("claude", 300000, "claude-3-7-sonnet", baseTime.Add(2*time.Hour))
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

func TestStore_AntigravityDailyWindow(t *testing.T) {
	s, err := backendstore.NewStore(t.TempDir())
	require.NoError(t, err)
	defer s.Close()

	day1 := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	// Record 800,000 tokens on Day 1 (80% used -> 20% headroom)
	err = s.RecordQuotaUsage("antigravity", 800000, "Gemini 3.1 Pro (High)", day1)
	require.NoError(t, err)

	headroom, used, limit, limited, err := s.GetHeadroom("antigravity", day1.Add(2*time.Hour))
	require.NoError(t, err)
	require.InDelta(t, 0.2, headroom, 0.001)
	require.Equal(t, 800000.0, used)
	require.Equal(t, 1000000.0, limit)
	require.False(t, limited)

	// Same day evening at 23:00 -> still 80% used
	headroom, used, _, _, err = s.GetHeadroom("antigravity", day1.Add(11*time.Hour))
	require.NoError(t, err)
	require.InDelta(t, 0.2, headroom, 0.001)
	require.Equal(t, 800000.0, used)

	// Next day (Day 2) at 00:05 UTC -> daily reset triggers!
	day2 := time.Date(2026, 8, 27, 0, 5, 0, 0, time.UTC)
	headroom, used, _, _, err = s.GetHeadroom("antigravity", day2)
	require.NoError(t, err)
	require.Equal(t, 1.0, headroom)
	require.Equal(t, 0.0, used)
}

func TestStore_CursorMonthlyWindow(t *testing.T) {
	s, err := backendstore.NewStore(t.TempDir())
	require.NoError(t, err)
	defer s.Close()

	august := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

	// Record 400 requests out of 500 in August (80% used -> 20% headroom)
	err = s.RecordQuotaUsage("cursor", 400, "sonnet-3.7", august)
	require.NoError(t, err)

	headroom, used, limit, _, err := s.GetHeadroom("cursor", august.Add(2*time.Hour))
	require.NoError(t, err)
	require.InDelta(t, 0.2, headroom, 0.001)
	require.Equal(t, 400.0, used)
	require.Equal(t, 500.0, limit)

	// Next month (September) -> reset!
	september := time.Date(2026, 9, 1, 1, 0, 0, 0, time.UTC)
	headroom, used, _, _, err = s.GetHeadroom("cursor", september)
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

	// During cooldown -> headroom = 0.0, limited = true
	headroom, _, _, limited, err := s.GetHeadroom("claude", now.Add(5*time.Minute))
	require.NoError(t, err)
	require.Equal(t, 0.0, headroom)
	require.True(t, limited)

	// After cooldown -> limited = false, headroom restored
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

	// Update quota limit for custom backend
	err = s.SetQuotaLimit("custom", 200000, backendstore.Window5HourRolling, 2*time.Hour)
	require.NoError(t, err)

	q, err := s.GetQuota("custom")
	require.NoError(t, err)
	require.Equal(t, "custom", q.BackendID)
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
