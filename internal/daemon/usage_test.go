package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/backendstore"
	"github.com/srjn45/warden/internal/backendusage"
	"github.com/stretchr/testify/require"
)

type usageRegistry struct{ rows []backendstore.Backend }

func (r usageRegistry) List() ([]backendstore.Backend, error) { return r.rows, nil }

type usageAdapter struct{}

func (usageAdapter) BackendID() string { return "codex" }
func (usageAdapter) Fetch(context.Context, backendstore.Backend) backendusage.Result {
	return backendusage.Result{BackendID: "codex", Status: backendusage.StatusOK, Usage: []backendusage.Limit{}, ObservedAt: time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)}
}

func TestUsageRouteReturnsTypedSnapshot(t *testing.T) {
	s := &Server{}
	s.SetUsageService(backendusage.NewService(usageRegistry{[]backendstore.Backend{{ID: "codex", Tier: backendstore.TierSubscription, Installed: true, Enabled: true}}}, usageAdapter{}))
	ts := httptest.NewServer(s.router())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/api/v1/usage?refresh=true")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestUsageRouteUnavailable(t *testing.T) {
	ts := httptest.NewServer((&Server{}).router())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/api/v1/usage")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}
