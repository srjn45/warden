package daemon

import (
	"context"

	"github.com/srjn45/warden/internal/relay"
	"github.com/srjn45/warden/relay/wire"
)

// SetRelayPolicy configures the accept-side policy for hub-opened relay streams
// (config relay.*). The zero-value policy rejects KindWebTerminated streams, so a
// daemon that never calls this stays closed to hub-terminated browser streams.
func (s *Server) SetRelayPolicy(p relay.Policy) { s.relayPolicy = p }

// AcceptRelayStream applies the daemon's relay policy to a hub-opened
// wire.StreamOpen and, on success, returns the request context to serve the
// stream's HTTP under. The returned context carries the grant's EFFECTIVE scope
// so per-request authorization — notably the /attach write-guard — applies to a
// relayed request exactly as it does to a token-authenticated one: a read-only
// grant reaches the same 403 that a read-only bearer token does.
//
// certImplied is the scope implied by the inner client certificate for a
// KindNativeE2E stream; pass wire.ScopeNone for KindWebTerminated (no inner cert).
// On rejection it returns a *relay.RejectError whose Code is the relay close code
// to send before tearing the stream down. This is the accept-side seam a future
// yamux connector calls once per opened stream; it is intentionally free of any
// transport so it can be exercised without a live relay.
func (s *Server) AcceptRelayStream(ctx context.Context, open wire.StreamOpen, certImplied wire.Scope) (context.Context, relay.Grant, error) {
	grant, err := s.relayPolicy.Accept(open, certImplied)
	if err != nil {
		return ctx, relay.Grant{}, err
	}
	return withRelayScope(ctx, relayAuthScope(grant.Scope)), grant, nil
}

// relayScopeCtxKey keys the relay-asserted auth scope carried on a relayed
// request's context. Distinct unexported type so it can't collide with another
// package's context value.
type relayScopeCtxKey struct{}

// withRelayScope returns a context carrying the relay-asserted auth scope. A
// relayed request served over an accepted stream runs under this scope instead of
// a bearer token (there is none — the hub already authenticated the user).
func withRelayScope(ctx context.Context, scope authScope) context.Context {
	return context.WithValue(ctx, relayScopeCtxKey{}, scope)
}

// relayScopeFromContext reports the relay-asserted auth scope on ctx, if any. ok
// is false for an ordinary (token-authenticated or loopback) request, which has
// no relay scope attached.
func relayScopeFromContext(ctx context.Context) (authScope, bool) {
	scope, ok := ctx.Value(relayScopeCtxKey{}).(authScope)
	return scope, ok
}

// relayAuthScope maps a wire.Scope (the hub-asserted, already-narrowed ceiling on
// a relay stream) onto the daemon's internal authScope. wire.ScopeReadOnly maps
// to scopeReadonly, which the write-guard denies the interactive attach — so a
// hub-asserted read-only grant cannot open a PTY, exactly like a read-only bearer
// token. An unrecognized/none scope authorizes nothing.
func relayAuthScope(s wire.Scope) authScope {
	switch s {
	case wire.ScopeFull:
		return scopeFull
	case wire.ScopeReadOnly:
		return scopeReadonly
	default:
		return scopeNone
	}
}
