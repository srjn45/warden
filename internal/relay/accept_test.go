package relay

import (
	"errors"
	"testing"

	"github.com/srjn45/warden/relay/wire"
)

// TestAcceptNarrowingFromFixtures drives Policy.Accept for KindNativeE2E through
// the shared streamopen.json scope-narrowing table so the daemon accept-side and
// the wire contract can never disagree on min(cert-implied, asserted).
func TestAcceptNarrowingFromFixtures(t *testing.T) {
	vecs, err := wire.LoadStreamOpenVectors()
	if err != nil {
		t.Fatalf("load streamopen vectors: %v", err)
	}
	if len(vecs.Narrowings) == 0 {
		t.Fatal("no narrowing vectors in fixture")
	}
	// AllowWebTerminated is irrelevant to native streams; set it to prove that.
	p := Policy{AllowWebTerminated: true}
	for _, n := range vecs.Narrowings {
		t.Run(n.Name, func(t *testing.T) {
			open := wire.StreamOpen{
				Version: wire.ProtoVersion,
				Kind:    wire.KindNativeE2E,
				Scope:   n.Asserted,
				Grantee: "user_native",
				CorrID:  "corr-native",
			}
			grant, err := p.Accept(open, n.CertImplied)
			if n.Effective == wire.ScopeNone {
				// A stream narrowed to nothing must be rejected as unauthorized, not
				// accepted as a zero-scope grant.
				var re *RejectError
				if !errors.As(err, &re) {
					t.Fatalf("expected RejectError for none-scope narrowing, got grant=%+v err=%v", grant, err)
				}
				if re.Code != wire.CloseUnauthorized {
					t.Fatalf("close code = %d, want %d (CloseUnauthorized)", re.Code, wire.CloseUnauthorized)
				}
				return
			}
			if err != nil {
				t.Fatalf("Accept returned error: %v", err)
			}
			if grant.Scope != n.Effective {
				t.Fatalf("effective scope = %d, want %d", grant.Scope, n.Effective)
			}
			if grant.Grantee != "user_native" || grant.CorrID != "corr-native" {
				t.Fatalf("grant identity not echoed: %+v", grant)
			}
		})
	}
}

// TestAcceptWebTerminatedGate is the N8 core: a KindWebTerminated StreamOpen is
// rejected with close code 4004 when relay.allow_web_terminated is off, and
// accepted honoring the hub-asserted {Grantee, Scope} when on.
func TestAcceptWebTerminatedGate(t *testing.T) {
	open := wire.StreamOpen{
		Version: wire.ProtoVersion,
		Kind:    wire.KindWebTerminated,
		Scope:   wire.ScopeFull,
		Grantee: "user_web",
		CorrID:  "corr-web-7",
	}

	// Disabled (default): reject with 4004.
	_, err := (Policy{AllowWebTerminated: false}).Accept(open, wire.ScopeNone)
	var re *RejectError
	if !errors.As(err, &re) {
		t.Fatalf("disabled: expected RejectError, got %v", err)
	}
	if re.Code != wire.CloseWebTerminatedDisabled {
		t.Fatalf("disabled: close code = %d, want %d (4004)", re.Code, wire.CloseWebTerminatedDisabled)
	}
	if wire.CloseWebTerminatedDisabled != 4004 {
		t.Fatalf("CloseWebTerminatedDisabled = %d, want 4004", wire.CloseWebTerminatedDisabled)
	}

	// Enabled: honor hub-asserted scope verbatim (no cert to narrow against, so
	// certImplied is ignored — pass ScopeNone to prove it is not consulted).
	grant, err := (Policy{AllowWebTerminated: true}).Accept(open, wire.ScopeNone)
	if err != nil {
		t.Fatalf("enabled: unexpected error: %v", err)
	}
	if grant.Scope != wire.ScopeFull {
		t.Fatalf("enabled: scope = %d, want ScopeFull (trusted verbatim)", grant.Scope)
	}
	if grant.Grantee != "user_web" || grant.CorrID != "corr-web-7" {
		t.Fatalf("enabled: identity not honored: %+v", grant)
	}
}

// TestAcceptWebTerminatedReadOnly proves that when web-terminated is enabled a
// hub-asserted ScopeReadOnly grant is honored AT ScopeReadOnly — the /attach
// write-guard denial (403) is then enforced per-request by the daemon, which maps
// this scope onto its read-only auth scope (see internal/daemon/relay_test.go).
func TestAcceptWebTerminatedReadOnly(t *testing.T) {
	open := wire.StreamOpen{
		Version: wire.ProtoVersion,
		Kind:    wire.KindWebTerminated,
		Scope:   wire.ScopeReadOnly,
		Grantee: "user_ro",
		CorrID:  "corr-ro",
	}
	grant, err := (Policy{AllowWebTerminated: true}).Accept(open, wire.ScopeNone)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if grant.Scope != wire.ScopeReadOnly {
		t.Fatalf("scope = %d, want ScopeReadOnly (not widened)", grant.Scope)
	}
}

// TestAcceptKindAndVersionRejects covers the remaining reject branches.
func TestAcceptKindAndVersionRejects(t *testing.T) {
	base := wire.StreamOpen{Version: wire.ProtoVersion, Scope: wire.ScopeFull, Grantee: "u"}
	cases := []struct {
		name string
		open wire.StreamOpen
		code wire.CloseCode
	}{
		{
			name: "unknown_kind",
			open: func() wire.StreamOpen { o := base; o.Kind = wire.StreamKind(99); return o }(),
			code: wire.CloseUnknownStreamKind,
		},
		{
			name: "invalid_kind_zero",
			open: func() wire.StreamOpen { o := base; o.Kind = wire.KindInvalid; return o }(),
			code: wire.CloseUnknownStreamKind,
		},
		{
			name: "control_inbound",
			open: func() wire.StreamOpen { o := base; o.Kind = wire.KindControl; return o }(),
			code: wire.CloseProtocolError,
		},
		{
			name: "version_zero",
			open: func() wire.StreamOpen { o := base; o.Kind = wire.KindNativeE2E; o.Version = 0; return o }(),
			code: wire.CloseUnsupportedVersion,
		},
		{
			name: "version_future",
			open: func() wire.StreamOpen {
				o := base
				o.Kind = wire.KindNativeE2E
				o.Version = wire.ProtoVersion + 1
				return o
			}(),
			code: wire.CloseUnsupportedVersion,
		},
	}
	p := Policy{AllowWebTerminated: true}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := p.Accept(tc.open, wire.ScopeFull)
			var re *RejectError
			if !errors.As(err, &re) {
				t.Fatalf("expected RejectError, got %v", err)
			}
			if re.Code != tc.code {
				t.Fatalf("close code = %d, want %d", re.Code, tc.code)
			}
		})
	}
}
