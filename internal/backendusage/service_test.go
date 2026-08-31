package backendusage

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/backendstore"
	"github.com/stretchr/testify/require"
)

type fakeRegistry struct {
	rows []backendstore.Backend
	err  error
}

func (f fakeRegistry) List() ([]backendstore.Backend, error) {
	return append([]backendstore.Backend(nil), f.rows...), f.err
}

type fakeAdapter struct {
	id    string
	fetch func(context.Context, backendstore.Backend) Result
}

func (f fakeAdapter) BackendID() string { return f.id }
func (f fakeAdapter) Fetch(ctx context.Context, b backendstore.Backend) Result {
	return f.fetch(ctx, b)
}

func TestServiceSelectsExactSubscriptionRowsAndSorts(t *testing.T) {
	now := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	reg := fakeRegistry{rows: []backendstore.Backend{{ID: "zeta", Tier: backendstore.TierSubscription, Installed: true, Enabled: false}, {ID: "free", Tier: backendstore.TierFree, Installed: true}, {ID: "alpha", Tier: backendstore.TierSubscription, Installed: true, Enabled: true}}}
	var mu sync.Mutex
	called := map[string]bool{}
	adapter := func(id string) Adapter {
		return fakeAdapter{id: id, fetch: func(_ context.Context, b backendstore.Backend) Result {
			mu.Lock()
			called[b.ID] = true
			mu.Unlock()
			return Result{BackendID: b.ID, Status: StatusOK, Usage: []Limit{}, ObservedAt: now}
		}}
	}
	s := NewService(reg, adapter("zeta"), adapter("alpha"))
	s.now = func() time.Time { return now }
	got, err := s.Snapshot(context.Background(), false)
	require.NoError(t, err)
	require.Equal(t, []string{"alpha", "zeta"}, []string{got.Backends[0].ID, got.Backends[1].ID})
	require.False(t, got.Backends[1].Enabled)
	require.Equal(t, map[string]bool{"alpha": true, "zeta": true}, called)
}

func TestServiceFreshCacheRefreshAndRedaction(t *testing.T) {
	now := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	calls := 0
	a := fakeAdapter{id: "codex", fetch: func(context.Context, backendstore.Backend) Result {
		calls++
		return Result{Status: StatusOK, Account: &Account{Plan: "plus", Label: "person@example.invalid"}, Usage: []Limit{}, ObservedAt: now}
	}}
	s := NewService(fakeRegistry{rows: []backendstore.Backend{{ID: "codex", Tier: backendstore.TierSubscription, Installed: true}}}, a)
	s.now = func() time.Time { return now }
	first, err := s.Snapshot(context.Background(), false)
	require.NoError(t, err)
	require.False(t, first.Backends[0].Cached)
	second, err := s.Snapshot(context.Background(), false)
	require.NoError(t, err)
	require.True(t, second.Backends[0].Cached)
	require.Empty(t, second.Backends[0].Account.Label)
	require.Equal(t, 1, calls)
	_, err = s.Snapshot(context.Background(), true)
	require.NoError(t, err)
	require.Equal(t, 2, calls)
}

func TestServicePreservesPartialResults(t *testing.T) {
	now := time.Now().UTC()
	ok := fakeAdapter{id: "a", fetch: func(context.Context, backendstore.Backend) Result {
		return Result{Status: StatusOK, Usage: []Limit{}, ObservedAt: now}
	}}
	bad := fakeAdapter{id: "b", fetch: func(context.Context, backendstore.Backend) Result {
		return Result{Status: StatusUnavailable, Usage: []Limit{}, ObservedAt: now, Error: &ProviderError{Code: "unavailable", Message: "probe unavailable"}}
	}}
	s := NewService(fakeRegistry{rows: []backendstore.Backend{{ID: "a", Tier: backendstore.TierSubscription}, {ID: "b", Tier: backendstore.TierSubscription}}}, ok, bad)
	got, err := s.Snapshot(context.Background(), false)
	require.NoError(t, err)
	require.Equal(t, StatusOK, got.Backends[0].Status)
	require.Equal(t, StatusUnavailable, got.Backends[1].Status)
}

func TestServiceSortsDistinctUsageLimitsWithoutFlattening(t *testing.T) {
	now := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	usedGemini, usedOther := 50.0, 20.0
	resetGemini := now.Add(2 * time.Hour)
	a := fakeAdapter{id: "antigravity", fetch: func(context.Context, backendstore.Backend) Result {
		return Result{Status: StatusOK, ObservedAt: now, Usage: []Limit{
			{ID: "antigravity:non-gemini", Scope: "non-gemini", Label: "Non-Gemini models", ModelFamilies: []string{"claude"}, UsedPercent: &usedOther},
			{ID: "antigravity:gemini", Scope: "gemini", Label: "Gemini models", ModelFamilies: []string{"gemini"}, UsedPercent: &usedGemini, ResetsAt: &resetGemini},
		}}
	}}
	s := NewService(fakeRegistry{rows: []backendstore.Backend{{ID: "antigravity", Tier: backendstore.TierSubscription, Installed: true}}}, a)
	got, err := s.Snapshot(context.Background(), false)
	require.NoError(t, err)
	require.Equal(t, []string{"antigravity:gemini", "antigravity:non-gemini"}, []string{got.Backends[0].Usage[0].ID, got.Backends[0].Usage[1].ID})
	require.Equal(t, "gemini", got.Backends[0].Usage[0].Scope)
	require.Nil(t, got.Backends[0].Usage[1].ResetsAt, "unknown reset must remain unknown")
}

func TestServiceRejectsDuplicateLimitIDsAndNullsInvalidPercent(t *testing.T) {
	invalid := 101.0
	adapter := func(limits []Limit) Adapter {
		return fakeAdapter{id: "codex", fetch: func(context.Context, backendstore.Backend) Result {
			return Result{Status: StatusOK, Usage: limits, ObservedAt: time.Now()}
		}}
	}
	reg := fakeRegistry{rows: []backendstore.Backend{{ID: "codex", Tier: backendstore.TierSubscription, Installed: true}}}
	s := NewService(reg, adapter([]Limit{{ID: "x", Scope: "all", Label: "All", UsedPercent: &invalid}}))
	got, err := s.Snapshot(context.Background(), false)
	require.NoError(t, err)
	require.Nil(t, got.Backends[0].Usage[0].UsedPercent)
	s = NewService(reg, adapter([]Limit{{ID: "x", Scope: "a", Label: "A"}, {ID: "x", Scope: "b", Label: "B"}}))
	got, err = s.Snapshot(context.Background(), false)
	require.NoError(t, err)
	require.Equal(t, StatusError, got.Backends[0].Status)
	require.Empty(t, got.Backends[0].Usage)
}

func TestServiceRegistryFailureProducesNoDocument(t *testing.T) {
	_, err := NewService(fakeRegistry{err: errors.New("boom")}).Snapshot(context.Background(), false)
	require.Error(t, err)
}
