// Package wire is the public, dependency-free contract shared by the warden
// daemon (github.com/srjn45/warden/internal/relay) and the warden-hub server for
// the reverse-tunnel relay described in docs/specs/2026-08-23-warden-hub.md.
//
// It contains ONLY on-the-wire types, enums, framing codecs, and version/domain
// constants — no daemon or hub logic, and no dependency on any internal/*
// package — so warden-hub (a separate module) can import it directly:
//
//	import "github.com/srjn45/warden/relay/wire"
//
// The relay has three legs:
//
//	Leg 0  Enrollment (plain REST): a daemon-holds-key CSR flow — EnrollRequest /
//	       EnrollResponse, EnrollmentToken{Request,Response}. The private key
//	       never transits the hub.
//	Leg 1  Daemon<->hub transport (WSS + yamux): the daemon dials out and proves
//	       its identity with AuthChallenge / AuthResponse (ECDSA over a
//	       domain-separated nonce digest), then serves as the yamux.Server.
//	Leg 2  Per-client streams: for each inbound client the hub (yamux.Client)
//	       opens a stream and writes a StreamOpen header (framed) before any
//	       client bytes; the daemon serves its whole HTTP API over the stream. A
//	       separate daemon-opened control stream carries Hello / Heartbeat / Bye /
//	       ConfigPush.
//
// Device authorization (`warden login`, DeviceStart/DeviceToken{Request,Response})
// is a SIBLING of Leg-0 enrollment, not a fourth relay leg: it is a second
// plain-REST, daemon-holds-key CSR provisioning path that mints a daemon identity
// before Leg 1 can be dialed. Enrollment consumes a one-time operator token; the
// device flow is the interactive browser-approved path. Both hand back a signed
// cert and CA chain and never a private key. See device.go.
//
// All framing uses one primitive: WriteFrame / ReadFrame (a uint32 big-endian
// length prefix followed by the payload). StreamOpen bodies are binary; control
// messages are JSON.
package wire
