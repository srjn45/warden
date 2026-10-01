package plansync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/srjn45/warden/internal/planstore"
	"github.com/stretchr/testify/require"
)

func TestHTTPService_HubProviderRoundTripConflictAndDiscoverDefaults(t *testing.T) {
	store, err := NewFileHubStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	service := HTTPService{Store: store}
	mux := http.NewServeMux()
	mux.HandleFunc(PathPush, service.Push)
	mux.HandleFunc(PathPull, service.Pull)
	mux.HandleFunc(PathDiscover, service.Discover)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	hub, err := NewHub(HubOptions{BaseURL: ts.URL, Token: "hub-token", HTTP: ts.Client()})
	require.NoError(t, err)
	ctx := context.Background()
	first := sampleEnvelope(t)
	require.NoError(t, hub.Push(ctx, first))

	got, err := hub.Pull(ctx, PullQuery{Scope: first.Scope, PlanID: first.PlanID})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.NotEmpty(t, got[0].RemoteID)
	require.NotNil(t, got[0].SyncedAt)

	stale := first
	stale.Revision++
	stale.ContentHash = "sha256:new"
	stale.ConflictToken = ConflictToken(stale.Revision, stale.ContentHash)
	err = hub.Push(ctx, stale)
	var conflict *ConflictError
	require.True(t, errors.As(err, &conflict))
	require.Equal(t, first.PlanID, conflict.PlanID)
	require.Equal(t, first.ConflictToken, conflict.Expected)
	require.Equal(t, stale.ConflictToken, conflict.Actual)

	completed := sampleEnvelope(t)
	completed.PlanID = "plan-completed"
	completed.Lifecycle = planstore.PlanStatusCompleted
	completed.ConflictToken = ConflictToken(completed.Revision, completed.ContentHash)
	require.NoError(t, hub.Push(ctx, completed))
	discovered, err := hub.Discover(ctx, first.Scope, nil)
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	require.Equal(t, first.PlanID, discovered[0].PlanID)

	// The Hub applies the same default even when a caller omits statuses rather
	// than relying on HubProvider to send its explicit convenience filter.
	body, err := json.Marshal(map[string]Scope{"scope": first.Scope})
	require.NoError(t, err)
	resp, err := http.Post(ts.URL+PathDiscover, "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var raw envelopesResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&raw))
	require.Len(t, raw.Envelopes, 1)
	require.Equal(t, first.PlanID, raw.Envelopes[0].PlanID)
}

func TestFileHubStore_PersistsEnvelopes(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFileHubStore(dir)
	require.NoError(t, err)
	first := sampleEnvelope(t)
	accepted, err := store.Push(context.Background(), first)
	require.NoError(t, err)
	require.NoError(t, store.Close())

	reopened, err := NewFileHubStore(dir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	got, err := reopened.Pull(context.Background(), PullQuery{Scope: first.Scope})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, accepted.RemoteID, got[0].RemoteID)
	require.Equal(t, accepted.ConflictToken, got[0].ConflictToken)
}
