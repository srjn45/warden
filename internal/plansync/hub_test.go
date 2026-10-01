package plansync

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/planstore"
)

func TestHubProvider_pushPullDiscoverAndStamp(t *testing.T) {
	fixed := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	var lastPush Envelope

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)

		switch r.URL.Path {
		case PathPush:
			require.Equal(t, http.MethodPost, r.Method)
			require.NoError(t, json.Unmarshal(body, &lastPush))
			resp := lastPush
			resp.RemoteID = "hub-remote-1"
			resp.SyncedAt = &fixed
			require.NoError(t, json.NewEncoder(w).Encode(resp))

		case PathPull:
			var req pullRequest
			require.NoError(t, json.Unmarshal(body, &req))
			require.Equal(t, lastPush.PlanID, req.PlanID)
			env := lastPush
			env.RemoteID = "hub-remote-1"
			env.SyncedAt = &fixed
			require.NoError(t, json.NewEncoder(w).Encode(envelopesResponse{Envelopes: []Envelope{env}}))

		case PathDiscover:
			var req discoverRequest
			require.NoError(t, json.Unmarshal(body, &req))
			require.Equal(t, []planstore.PlanStatus{
				planstore.PlanStatusPending, planstore.PlanStatusInProgress,
			}, req.Statuses)
			env := lastPush
			env.RemoteID = "hub-remote-1"
			env.SyncedAt = &fixed
			require.NoError(t, json.NewEncoder(w).Encode(envelopesResponse{Envelopes: []Envelope{env}}))

		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	h, err := NewHub(HubOptions{
		BaseURL: srv.URL,
		Token:   "test-token",
		HTTP:    srv.Client(),
		Now:     func() time.Time { return fixed },
	})
	require.NoError(t, err)
	require.Equal(t, ProviderHub, h.Name())
	require.True(t, h.Enabled())

	ctx := context.Background()
	env := sampleEnvelope(t)
	require.NoError(t, h.Push(ctx, env))

	plan := samplePlan()
	require.Nil(t, plan.SyncedAt)
	require.Empty(t, plan.RemoteID)
	require.NoError(t, h.SyncPushPlan(ctx, plan, EnvelopeOptions{
		Scope:      env.Scope,
		Visibility: env.Visibility,
		OwnerID:    env.OwnerID,
		Origin:     env.Origin,
	}))
	require.Equal(t, "hub-remote-1", plan.RemoteID)
	require.NotNil(t, plan.SyncedAt)
	require.True(t, plan.SyncedAt.Equal(fixed))

	got, err := h.Pull(ctx, PullQuery{Scope: env.Scope, PlanID: env.PlanID})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "hub-remote-1", got[0].RemoteID)

	disc, err := h.Discover(ctx, env.Scope, nil)
	require.NoError(t, err)
	require.Len(t, disc, 1)

	plan2 := samplePlan()
	require.NoError(t, h.SyncPullPlan(ctx, plan2, env.Scope))
	require.Equal(t, "hub-remote-1", plan2.RemoteID)
	require.NotNil(t, plan2.SyncedAt)
}

func TestHubProvider_conflict(t *testing.T) {
	var received Envelope
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, PathPush, r.URL.Path)
		require.Equal(t, "Bearer t", r.Header.Get("Authorization"))
		require.NoError(t, json.NewDecoder(r.Body).Decode(&received))
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"conflict","expected":"3:sha256:abc","actual":"4:sha256:def"}`))
	}))
	t.Cleanup(srv.Close)

	h, err := NewHub(HubOptions{BaseURL: srv.URL, Token: "t", HTTP: srv.Client()})
	require.NoError(t, err)

	env := sampleEnvelope(t)
	err = h.Push(context.Background(), env)
	require.Error(t, err)
	require.Equal(t, env.ConflictToken, received.ConflictToken, "Hub receives the OCC token unchanged")
	var cerr *ConflictError
	require.True(t, errors.As(err, &cerr))
	require.Equal(t, "plan-cafebabe", cerr.PlanID)
	require.Equal(t, "3:sha256:abc", cerr.Expected)
	require.ErrorIs(t, err, ErrConflict)
}

func TestHubProvider_discoverSendsExplicitFilters(t *testing.T) {
	requested := make(chan discoverRequest, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, PathDiscover, r.URL.Path)
		require.Equal(t, "Bearer t", r.Header.Get("Authorization"))
		var req discoverRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		requested <- req
		require.NoError(t, json.NewEncoder(w).Encode(envelopesResponse{}))
	}))
	t.Cleanup(srv.Close)

	h, err := NewHub(HubOptions{BaseURL: srv.URL, Token: "t", HTTP: srv.Client()})
	require.NoError(t, err)
	scope := Scope{OrganizationID: "org-1", TeamID: "team-1", ProjectID: "proj-1"}
	statuses := []planstore.PlanStatus{planstore.PlanStatusCompleted, planstore.PlanStatusArchived}
	got, err := h.Discover(context.Background(), scope, statuses)
	require.NoError(t, err)
	require.Empty(t, got)

	req := <-requested
	require.Equal(t, scope, req.Scope)
	require.Equal(t, statuses, req.Statuses, "explicit lifecycle filters must not be replaced by discovery defaults")
}

func TestHubProvider_disabledWithoutToken(t *testing.T) {
	h, err := NewHub(HubOptions{BaseURL: "http://127.0.0.1:9", Token: ""})
	require.NoError(t, err)
	require.False(t, h.Enabled())
	err = h.Push(context.Background(), sampleEnvelope(t))
	require.ErrorIs(t, err, ErrDisabled)
}

func TestHubProvider_requiresBaseURL(t *testing.T) {
	_, err := NewHub(HubOptions{})
	require.Error(t, err)
}

func TestStampPlan_onlyWithRemoteID(t *testing.T) {
	p := samplePlan()
	StampPlan(p, "", time.Now())
	require.Nil(t, p.SyncedAt)
	require.Empty(t, p.RemoteID)

	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	StampPlan(p, "rid-1", at)
	require.Equal(t, "rid-1", p.RemoteID)
	require.True(t, p.SyncedAt.Equal(at))

	env := Envelope{RemoteID: "rid-2", SyncedAt: &at}
	StampPlanFromEnvelope(p, env)
	require.Equal(t, "rid-2", p.RemoteID)
}

func TestNew_localDefaultAndHub(t *testing.T) {
	p, err := New(ClientConfig{})
	require.NoError(t, err)
	require.Equal(t, ProviderLocal, p.Name())
	require.False(t, p.Enabled())

	_, err = New(ClientConfig{Provider: "hub"})
	require.Error(t, err, "hub without base URL")

	h, err := New(ClientConfig{
		Provider: "hub",
		BaseURL:  "http://127.0.0.1:9",
		Token:    "tok",
		HTTP:     &http.Client{Timeout: time.Millisecond},
	})
	require.NoError(t, err)
	require.Equal(t, ProviderHub, h.Name())
	require.True(t, h.Enabled())

	_, err = New(ClientConfig{Provider: "nope"})
	require.Error(t, err)
}

func TestDefault_stillLocal(t *testing.T) {
	require.Equal(t, ProviderLocal, Default().Name())
	require.False(t, Default().Enabled())
}

func TestHubProvider_contract(t *testing.T) {
	fixed := time.Date(2026, 10, 1, 15, 0, 0, 0, time.UTC)
	store := map[string]Envelope{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case PathPush:
			var env Envelope
			require.NoError(t, json.NewDecoder(r.Body).Decode(&env))
			if prev, ok := store[env.PlanID]; ok && prev.ConflictToken != env.ConflictToken {
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(hubErrorBody{
					Error: "conflict", PlanID: env.PlanID,
					Expected: prev.ConflictToken, Actual: env.ConflictToken,
				})
				return
			}
			env.RemoteID = "hub-" + env.PlanID
			env.SyncedAt = &fixed
			store[env.PlanID] = env
			_ = json.NewEncoder(w).Encode(env)
		case PathPull:
			var req pullRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			var out []Envelope
			for _, env := range store {
				if req.PlanID != "" && env.PlanID != req.PlanID {
					continue
				}
				if !matchScope(env, req.Scope) {
					continue
				}
				if !matchStatuses(env.Lifecycle, req.Statuses) {
					continue
				}
				out = append(out, env)
			}
			_ = json.NewEncoder(w).Encode(envelopesResponse{Envelopes: out})
		case PathDiscover:
			var req discoverRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			var out []Envelope
			for _, env := range store {
				if !matchScope(env, req.Scope) {
					continue
				}
				if !matchStatuses(env.Lifecycle, req.Statuses) {
					continue
				}
				out = append(out, env)
			}
			_ = json.NewEncoder(w).Encode(envelopesResponse{Envelopes: out})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	h, err := NewHub(HubOptions{BaseURL: srv.URL, Token: "t", HTTP: srv.Client(), Now: func() time.Time { return fixed }})
	require.NoError(t, err)
	runProviderContract(t, h)
}
