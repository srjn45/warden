package backendusage

import (
	"testing"
	"time"

	"github.com/srjn45/warden/internal/backendstore"
	"github.com/stretchr/testify/require"
)

type mockQuotaWriter struct {
	quotas  []backendstore.BackendQuota
	limited map[string]time.Time
	setErr  error
	limErr  error
}

func (m *mockQuotaWriter) SetQuota(q backendstore.BackendQuota) error {
	if m.setErr != nil {
		return m.setErr
	}
	m.quotas = append(m.quotas, q)
	return nil
}

func (m *mockQuotaWriter) SetLimited(backendID string, until time.Time) error {
	if m.limErr != nil {
		return m.limErr
	}
	if m.limited == nil {
		m.limited = map[string]time.Time{}
	}
	m.limited[backendID] = until
	return nil
}

func pct(v float64) *float64 { return &v }

func state(s string) *string { return &s }

func TestSyncToStore_WritesScopedRecords(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	reset := now.Add(24 * time.Hour)
	w := &mockQuotaWriter{}
	snap := Snapshot{
		GeneratedAt: now,
		Backends: []BackendResult{{
			ID: "cursor", Status: StatusOK,
			Usage: []Limit{
				{ID: "cursor:api", Scope: "api", Label: "API", UsedPercent: pct(100), ResetsAt: &reset, LimitState: state("reached")},
				{ID: "cursor:auto", Scope: "auto", Label: "Auto", UsedPercent: pct(45), ResetsAt: &reset},
				{ID: "cursor:included", Scope: "included", Label: "Included", UsedPercent: pct(10), ResetsAt: &reset},
			},
		}},
	}
	require.NoError(t, SyncToStore(snap, w))
	require.Len(t, w.quotas, 3)

	byScope := map[string]backendstore.BackendQuota{}
	for _, q := range w.quotas {
		require.Equal(t, "cursor", q.BackendID)
		require.Equal(t, SyntheticQuotaLimit, q.QuotaLimit)
		require.Equal(t, now, q.UpdatedAt)
		byScope[q.Scope] = q
	}
	require.InDelta(t, 100.0, byScope["api"].UsedAmount, 0.001)
	require.Equal(t, reset, byScope["api"].LimitedUntil)
	require.InDelta(t, 45.0, byScope["auto"].UsedAmount, 0.001)
	require.True(t, byScope["auto"].LimitedUntil.IsZero())
	require.InDelta(t, 10.0, byScope["included"].UsedAmount, 0.001)

	// Not all scopes limited → backend LimitedUntil cleared.
	require.Contains(t, w.limited, "cursor")
	require.True(t, w.limited["cursor"].IsZero())
}

func TestSyncToStore_SetsBackendLimitedOnlyWhenAllScopesLimited(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	untilAPI := now.Add(1 * time.Hour)
	untilAuto := now.Add(2 * time.Hour)
	untilInc := now.Add(3 * time.Hour)
	w := &mockQuotaWriter{}
	snap := Snapshot{
		GeneratedAt: now,
		Backends: []BackendResult{{
			ID: "cursor", Status: StatusRateLimited,
			Usage: []Limit{
				{ID: "cursor:api", Scope: "api", Label: "API", UsedPercent: pct(100), ResetsAt: &untilAPI, LimitState: state("rate_limited")},
				{ID: "cursor:auto", Scope: "auto", Label: "Auto", UsedPercent: pct(100), ResetsAt: &untilAuto, LimitState: state("reached")},
				{ID: "cursor:included", Scope: "included", Label: "Included", UsedPercent: pct(100), ResetsAt: &untilInc, LimitState: state("reached")},
			},
		}},
	}
	require.NoError(t, SyncToStore(snap, w))
	require.Len(t, w.quotas, 3)
	require.Equal(t, untilInc, w.limited["cursor"], "backend LimitedUntil is the latest per-scope reset")
}

func TestSyncToStore_ClearsBackendLimitedWhenScopeRecovers(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	until := now.Add(time.Hour)
	w := &mockQuotaWriter{limited: map[string]time.Time{"cursor": until}}
	snap := Snapshot{
		GeneratedAt: now,
		Backends: []BackendResult{{
			ID: "cursor", Status: StatusOK,
			Usage: []Limit{
				{ID: "cursor:api", Scope: "api", Label: "API", UsedPercent: pct(100), LimitState: state("reached"), ResetsAt: &until},
				{ID: "cursor:auto", Scope: "auto", Label: "Auto", UsedPercent: pct(20)},
			},
		}},
	}
	require.NoError(t, SyncToStore(snap, w))
	require.True(t, w.limited["cursor"].IsZero())
}

func TestSyncToStore_SkipsNonOKBackends(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	w := &mockQuotaWriter{}
	snap := Snapshot{
		GeneratedAt: now,
		Backends: []BackendResult{
			{ID: "claude", Status: StatusTimeout, Usage: []Limit{{ID: "x", Scope: "session", Label: "S", UsedPercent: pct(50)}}},
			{ID: "codex", Status: StatusUnsupported, Usage: []Limit{}},
		},
	}
	require.NoError(t, SyncToStore(snap, w))
	require.Empty(t, w.quotas)
	require.Empty(t, w.limited)
}

func TestSyncToStore_NilStoreNoop(t *testing.T) {
	require.NoError(t, SyncToStore(Snapshot{}, nil))
}

func TestSyncToStore_MultiWindowSameScope(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	d5h, dWeek := 300, 10080
	w := &mockQuotaWriter{}
	snap := Snapshot{
		GeneratedAt: now,
		Backends: []BackendResult{{
			ID: "antigravity", Status: StatusOK,
			Usage: []Limit{
				{ID: "gemini-5h", Scope: "gemini", Label: "Gemini 5h", UsedPercent: pct(100), DurationMinutes: &d5h, LimitState: state("reached")},
				{ID: "gemini-week", Scope: "gemini", Label: "Gemini week", UsedPercent: pct(40), DurationMinutes: &dWeek},
				{ID: "other-5h", Scope: "non-gemini", Label: "Other 5h", UsedPercent: pct(10), DurationMinutes: &d5h},
			},
		}},
	}
	require.NoError(t, SyncToStore(snap, w))
	require.Len(t, w.quotas, 3)
	require.Equal(t, backendstore.Window5HourRolling, w.quotas[0].WindowType)
	require.Equal(t, backendstore.WindowWeekly, w.quotas[1].WindowType)
	// gemini scope has one recovered window → not all scopes limited → clear.
	require.True(t, w.limited["antigravity"].IsZero())
}
