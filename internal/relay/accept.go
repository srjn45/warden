// Package relay is the warden daemon's node-side of the warden-hub reverse-tunnel
// relay (see docs/specs/2026-08-23-warden-hub.md and the shared wire contract in
// github.com/srjn45/warden/relay/wire). It holds two things the daemon needs
// before a full yamux connector exists:
//
//   - the accept-side policy that decides whether a hub-opened wire.StreamOpen is
//     honored, and at what effective scope (see accept.go); and
//   - the device-authorization client behind `warden login`, which provisions the
//     daemon's relay identity (a daemon-holds-key CSR flow — see login.go).
//
// It deliberately depends only on relay/wire and the standard library, never on
// internal/daemon, so the daemon can import it without a cycle.
package relay

import (
	"fmt"

	"github.com/srjn45/warden/relay/wire"
)

// Policy is the daemon's local relay accept-side policy. Its zero value is the
// safe default: KindWebTerminated streams are rejected until an operator opts in
// (config relay.allow_web_terminated).
type Policy struct {
	// AllowWebTerminated permits KindWebTerminated streams — hub-TLS-terminated
	// browser streams the daemon cannot cryptographically verify, so it trusts the
	// hub-asserted {Grantee, Scope}. Off by default; when off, such a StreamOpen is
	// rejected with wire.CloseWebTerminatedDisabled (4004).
	AllowWebTerminated bool
}

// Grant is the outcome of accepting a hub-opened stream: the authorized identity
// and the EFFECTIVE scope the daemon must enforce for every request served over
// the stream. For KindNativeE2E the scope has already been narrowed to
// min(cert-implied, hub-asserted); for KindWebTerminated it is the hub-asserted
// scope trusted verbatim.
type Grant struct {
	Kind    wire.StreamKind
	Grantee string
	Scope   wire.Scope
	CorrID  string
}

// RejectError is returned by Accept when a StreamOpen must be refused. It carries
// the relay-level close code the daemon should send (in a Bye / websocket close
// frame) before tearing the stream down.
type RejectError struct {
	Code   wire.CloseCode
	Reason string
}

func (e *RejectError) Error() string {
	return fmt.Sprintf("relay: reject StreamOpen (%d): %s", e.Code, e.Reason)
}

// reject is a small constructor so the switch below reads as a table.
func reject(code wire.CloseCode, reason string) (Grant, error) {
	return Grant{}, &RejectError{Code: code, Reason: reason}
}

// Accept applies the policy to a hub-opened wire.StreamOpen and returns the
// authorized Grant or a *RejectError carrying the close code to send.
//
// certImplied is the scope implied by the INNER client certificate on a
// KindNativeE2E stream (the daemon verifies that cert itself against the per-user
// CA). It is IGNORED for KindWebTerminated, which has no inner cert — pass
// wire.ScopeNone there.
//
// The security-critical rule is that a relay header can only ever NARROW
// authority, never widen it:
//
//   - KindNativeE2E: effective = min(cert-implied, StreamOpen.Scope). A buggy or
//     compromised hub cannot escalate a native client past what its cert allows.
//   - KindWebTerminated: there is no inner cert to narrow against, so the daemon
//     trusts StreamOpen.Scope outright — which is the whole reason the mode is
//     gated behind AllowWebTerminated and rejected with 4004 when off.
//
// A stream whose effective scope is wire.ScopeNone authorizes nothing and is
// rejected with wire.CloseUnauthorized rather than accepted as a useless grant.
func (p Policy) Accept(open wire.StreamOpen, certImplied wire.Scope) (Grant, error) {
	// Version: 0 never rides the wire, and a version newer than we implement means
	// no common framing to fall back on. Additive fields do not bump ProtoVersion,
	// so any value in [1, ProtoVersion] is speakable.
	if open.Version == 0 || open.Version > wire.ProtoVersion {
		return reject(wire.CloseUnsupportedVersion,
			fmt.Sprintf("unsupported wire version %d (daemon speaks %d)", open.Version, wire.ProtoVersion))
	}

	switch open.Kind {
	case wire.KindNativeE2E:
		eff := wire.NarrowScope(certImplied, open.Scope)
		if eff == wire.ScopeNone {
			return reject(wire.CloseUnauthorized, "native stream narrowed to no scope")
		}
		return Grant{Kind: open.Kind, Grantee: open.Grantee, Scope: eff, CorrID: open.CorrID}, nil

	case wire.KindWebTerminated:
		if !p.AllowWebTerminated {
			return reject(wire.CloseWebTerminatedDisabled,
				"web-terminated streams disabled (set relay.allow_web_terminated=true to opt in)")
		}
		if open.Scope == wire.ScopeNone {
			return reject(wire.CloseUnauthorized, "web-terminated stream asserted no scope")
		}
		// No inner cert to verify: trust the hub-asserted scope verbatim.
		return Grant{Kind: open.Kind, Grantee: open.Grantee, Scope: open.Scope, CorrID: open.CorrID}, nil

	case wire.KindControl:
		// The control stream is DAEMON-opened; the hub must never open one to the
		// daemon. Receiving it inbound is a protocol violation, not an auth failure.
		return reject(wire.CloseProtocolError, "control stream must be daemon-opened")

	default:
		return reject(wire.CloseUnknownStreamKind, fmt.Sprintf("unknown stream kind %d", open.Kind))
	}
}
