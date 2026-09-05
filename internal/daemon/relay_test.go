package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/relay"
	"github.com/srjn45/warden/relay/wire"
)

// TestRelayAuthScopeMapping pins the wire.Scope → authScope translation.
func TestRelayAuthScopeMapping(t *testing.T) {
	require.Equal(t, scopeFull, relayAuthScope(wire.ScopeFull))
	require.Equal(t, scopeReadonly, relayAuthScope(wire.ScopeReadOnly))
	require.Equal(t, scopeNone, relayAuthScope(wire.ScopeNone))
}

// TestAcceptRelayStreamGate is the N8 daemon-side gate: a KindWebTerminated
// StreamOpen is rejected with close code 4004 unless relay.allow_web_terminated
// is set, and honored at the hub-asserted scope when it is.
func TestAcceptRelayStreamGate(t *testing.T) {
	open := wire.StreamOpen{
		Version: wire.ProtoVersion,
		Kind:    wire.KindWebTerminated,
		Scope:   wire.ScopeFull,
		Grantee: "user_web",
		CorrID:  "corr-web",
	}

	// Default policy (zero value): web-terminated disabled → reject 4004.
	off := &Server{}
	_, _, err := off.AcceptRelayStream(context.Background(), open, wire.ScopeNone)
	var re *relay.RejectError
	require.ErrorAs(t, err, &re)
	require.Equal(t, wire.CloseWebTerminatedDisabled, re.Code)

	// Opted in: accepted, and the returned context carries the full scope.
	on := &Server{}
	on.SetRelayPolicy(relay.Policy{AllowWebTerminated: true})
	ctx, grant, err := on.AcceptRelayStream(context.Background(), open, wire.ScopeNone)
	require.NoError(t, err)
	require.Equal(t, wire.ScopeFull, grant.Scope)
	scope, ok := relayScopeFromContext(ctx)
	require.True(t, ok)
	require.Equal(t, scopeFull, scope)
}

// TestRelayWebTerminatedReadOnlyCannotAttach is the invariant the spec calls out:
// a hub-asserted ScopeReadOnly web-terminated grant must NOT be able to open the
// interactive PTY attach — /attach stays 403 — while reads pass and a full grant
// attaches. It drives the REAL authMiddleware so the relay path reuses the exact
// per-request enforcement a read-only bearer token gets, rather than a parallel
// check that could drift.
func TestRelayWebTerminatedReadOnlyCannotAttach(t *testing.T) {
	srv := &Server{}
	srv.SetRelayPolicy(relay.Policy{AllowWebTerminated: true})

	var reached bool
	h := srv.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))

	// serve simulates the future connector: accept the stream, then run the
	// relayed request through the gated route group under the grant's context.
	serve := func(method, path string, scope wire.Scope) int {
		reached = false
		open := wire.StreamOpen{Version: wire.ProtoVersion, Kind: wire.KindWebTerminated, Scope: scope, Grantee: "u"}
		ctx, _, err := srv.AcceptRelayStream(context.Background(), open, wire.ScopeNone)
		require.NoError(t, err)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, nil).WithContext(ctx)
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	// Read-only grant: reads pass, attach and writes are forbidden.
	require.Equal(t, http.StatusOK, serve(http.MethodGet, "/api/v1/sessions", wire.ScopeReadOnly))
	require.True(t, reached)
	require.Equal(t, http.StatusForbidden, serve(http.MethodGet, "/api/v1/sessions/s1/attach", wire.ScopeReadOnly))
	require.False(t, reached, "read-only relay grant must not reach the attach handler")
	require.Equal(t, http.StatusForbidden, serve(http.MethodGet, "/api/v1/cockpit/attach", wire.ScopeReadOnly))
	require.False(t, reached)
	require.Equal(t, http.StatusForbidden, serve(http.MethodPost, "/api/v1/spawn", wire.ScopeReadOnly))
	require.False(t, reached)

	// Full grant: attach reaches the handler.
	require.Equal(t, http.StatusOK, serve(http.MethodGet, "/api/v1/sessions/s1/attach", wire.ScopeFull))
	require.True(t, reached)
}
