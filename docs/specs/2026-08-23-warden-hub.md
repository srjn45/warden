# Warden Hub — Central Rendezvous, Identity & Relay (Option-1 Build)

**Date:** 2026-08-23
**Status:** Design / planning. No code yet. Brainstorm-derived; supersedes the
"do we need a browser/screen-share for remote access?" thread (answer: no — see
`guides/reauth-from-phone.md`) and the Tailscale-dependent portions of remote
access (`guides/remote-access.md`).

---

## 1. Goal & scope

Let a user reach their warden daemon **over the public internet, without a VPN**,
from any warden client (CLI, TUI, web, Android). Replace the Tailscale dependency
with an in-house control plane — **warden-hub** — that provides real account
authentication, per-user authorization, daemon registry + presence, and a byte
relay that traverses NAT.

The daemon sits behind NAT and **cannot accept inbound connections**. So the
daemon dials *out* to the hub and holds a persistent channel open; clients also
connect to the hub; the hub matches them by user + daemon-id and relays bytes.
This is the Tailscale-DERP / Cloudflare-Tunnel / VS-Code-Remote-Tunnels model.

### 1.1 Core invariant — the daemon never depends on the hub

> **Every daemon change in this spec is opt-in and zero-config by default.** The
> relay connector activates *only* when `hub.url` + credentials are configured.
> A fresh `warden` install stays exactly what it is today — a one-command local
> agent-manager on loopback/LAN. The hub and the Android app are **bolt-ons,
> never dependencies**: a user can install and run warden forever and never know
> the hub exists. Nothing in this feature may add a required setup step,
> background connection, or config to the default local experience.

This invariant governs every implementation decision below; if a change would
make the plain local daemon need the hub, the change is wrong.

### 1.2 Product posture — Option C (chosen)

- **warden daemon** → open source. The engine + all clients that run on the
  user's own box. Fully useful standalone (LAN/loopback), no hub required.
- **warden-hub** → **private, hosted-only** (not open source, not self-hostable).
- **Android app** → private eventually (parked; see §13).

Because the hub is closed, it is **not** self-hostable — but §11 explains why the
E2E design keeps a private hosted hub trustworthy anyway.

### 1.3 Trust model — Option 1 (tiered E2E)

- **Native clients (CLI / TUI / Android): true end-to-end mTLS.** Daemon and
  client hold certs from a per-user CA and mutually authenticate *through* the
  hub. The hub relays ciphertext — it cannot read agent I/O or inject input.
- **Web client: hub-terminated TLS (trusted for that session only).** Browsers
  cannot present a custom-CA client cert or run the native handshake from JS, so
  the hub terminates the browser's TLS and re-encrypts to the daemon over the
  authenticated tunnel.

Option-2 ("zero-knowledge everywhere", incl. web E2E via in-browser WASM crypto)
is a **strict superset** of this build — it shares 100% of the native + hub + CA
work and only *replaces the web client's transport*. It is flagged throughout as
a documented phase-2 upgrade (§14). Choosing option-1 now does not burn the
bridge to option-2 later.

### 1.4 Non-goals (this spec)

- In-browser E2E (option-2 web) — deferred, §14.
- A `browser` session kind / screen-share / CDP screencast — a *separate* need
  (viewing a local web app), unrelated to reaching the daemon; parked.
- Replacing LAN/loopback access. The hub is **additive**; existing direct-access
  paths keep working unchanged (§7).
- Open-sourcing or self-hosting the hub (Option C is hosted-only).

---

## 2. Key insight — the whole daemon API rides for free

The entire daemon API is a single `http.Handler` (`s.router()`) served over a
`net.Listener` (`internal/daemon/server.go:166`). Relaying it therefore touches
**zero routes**: we hand the same handler connections that arrive from the hub
instead of from a local TCP socket.

A stream multiplexer (`yamux`/`smux`) over the daemon⇄hub socket exposes exactly
a `net.Listener` yielding `net.Conn`s. So the daemon side is essentially:

```go
// new component: relay connector (outbound, ~one contained package)
wss  := dialHubAuthenticated(cfg)     // WSS to hub, authenticated with daemon cert
sess := yamux.Server(wss, yamuxCfg)   // one socket → many virtual streams
httpSrv.Serve(sess)                   // the SAME s.router(): REST, SSE, /attach — all of it
```

Every existing route — `/sessions`, `/events/stream` (SSE), `/sessions/{id}/attach`
and `/cockpit/attach` (PTY), schedules, everything — works over the tunnel with
**no per-route changes**. The relay is a new *front door*, not a new API.

> Naming note: `internal/daemon/hub.go` is the in-process SSE broadcaster —
> unrelated. The new daemon package is `internal/relay`. The hub *service* is its
> own repo (§13). Do **not** call the daemon component "hub".

---

## 3. Topology

```
   dev box (NAT)                       warden-hub (public, :443)                client
 ┌───────────────┐                   ┌────────────────────────────┐      ┌────────────────────┐
 │ warden daemon │  ── WSS out ──►   │ accounts │ daemon registry  │ ◄─WSS│ CLI / TUI / android │
 │  s.router()   │  (persistent,     │ presence │ rendezvous+relay │ ──►  │  (mTLS E2E)         │
 │  :8765 local  │   mux, mTLS-E2E)  │ per-user CA │ rate-limits    │      ├────────────────────┤
 └───────────────┘                   └────────────────────────────┘ ◄─WSS│ web (hub-terminated)│
        ▲                                                            ──►  └────────────────────┘
        └───────── logical request/attach stream, E2E for native clients ──────────┘
```

- **Outer transport: WSS over 443.** Universally egress-friendly; the daemon can
  always reach out even from hostile networks (inbound is impossible).
- **Inner: yamux/smux.** One daemon⇄hub socket carries many client connections as
  virtual streams. Each REST call, SSE stream, or `/attach` PTY = one stream.
- **Native E2E:** daemon and client run mTLS *inside* the relayed stream (hub sees
  ciphertext). **Web:** hub terminates client TLS, re-originates to the daemon leg.

---

## 4. Components

### 4.1 warden-hub (new private service)

Standalone Go service (private, hosted-only; §11, §12). Responsibilities:

- **Accounts / authN** — signup, login, session issuance (delegated to an auth
  provider; §12). Issues short-lived JWT access + refresh (web/app) and
  long-lived **device tokens** (CLI/TUI).
- **Daemon registry** — one row per enrolled daemon: `daemon_id`, owner
  `user_id`, friendly name, enrolled public key / cert serial, created/last-seen.
- **Presence** — heartbeat over the persistent daemon socket → "which of my
  daemons are online" in every client.
- **Per-user CA** — issues client certs (daemon + native clients) rooted in a
  per-user intermediate. Revocation list. This is the identity backbone of E2E.
- **Rendezvous + relay** — accepts the daemon's outbound persistent socket and
  client connections; authorizes the pairing (owner/grant check); bridges bytes.
- **Authorization** — owner → full; explicit **grants** → scoped (e.g. read-only,
  see §6.3). The hub is the authZ authority for *remote* access.
- **Abuse controls** — per-account connection caps, bandwidth quotas, rate limits
  (a public relay is a DoS + bandwidth surface).

### 4.2 Daemon relay connector (`internal/relay`, new)

- Dials the hub over WSS, authenticates with the enrolled daemon cert.
- Maintains the persistent mux socket: heartbeat, reconnect w/ capped backoff.
- For each inbound virtual stream: runs the mTLS server handshake (native peer)
  or accepts a hub-terminated stream (web), then serves it with `s.router()`.
- Config block (`hub.url`, credential path), lifecycle wired into
  `Server.ListenAndServe` alongside the existing background goroutines — **gated
  entirely on hub config being present** (§1.1 invariant).

### 4.3 Clients

- **CLI/TUI/Android:** `warden hub login` → list my daemons → connect to `D`.
  Anything that today targets `http://localhost:8765` or `--daemon <url>` gains a
  `hub://<daemon-id>` target routed through the relay with mTLS. Downstream of
  "I have a connection to the daemon", nothing changes.
- **Web:** login to the hub, pick a daemon, hub bridges (terminated). The SPA's
  existing `fetch`/`WebSocket`/SSE calls are unchanged — they just hit the hub.

---

## 5. Identity & enrollment flow

```
1. User creates a hub account.                       (authN root)
2. Hub → user: a one-time, scoped ENROLLMENT TOKEN.
3. On the dev box:  warden hub register --token <one-time>
     → daemon generates a keypair, sends a CSR, hub's per-user CA returns a
       daemon client cert (long-lived, revocable). Stored 0600 in ~/.warden/.
     → daemon is now a row in the registry, bound to the account.
4. Native client:   warden hub login  → account session → hub issues a client
       cert (or short-lived client credential) from the same per-user CA.
5. Connect:  client asks hub for daemon D → hub authorizes (owner/grant) →
       bridges the two legs → client and daemon run mTLS end-to-end through it.
```

- Enrollment tokens are **one-time, short-TTL, single-daemon** — leak-resistant.
- Long-lived identity is the **cert**, not a shared secret. Individually
  revocable (revoke a device without rotating anyone else). Rotation supported.
- The hub **never sees a bearer secret** for native clients — identity is the
  cert, and the E2E handshake means the hub can't read the session either.

---

## 6. Auth model rework — hub as the authZ authority (the important redesign)

Today the daemon has **two** trust boundaries (`internal/daemon/middleware.go`):

1. **Loopback position** — no token set → loopback-only; `hostGuard` defends
   against DNS rebinding. Frictionless local dev.
2. **Shared bearer token** — `WARDEN_TOKEN` (full) / `WARDEN_READONLY_TOKEN`
   (read scope) → required for any non-loopback (LAN/WAN) access.

Introducing the hub adds a **third, stronger boundary** and lets us stop
conflating "remote access" with "shared token".

### 6.1 The three trust boundaries (revised)

| Path | Who | Trust basis | Scope |
|---|---|---|---|
| **Loopback / same-host** | local CLI on the dev box | network position (unchanged) | full |
| **Direct LAN/WAN** | another device hitting `:8765` directly | **shared bearer token** (unchanged) | full / readonly |
| **Hub relay (NEW)** | remote via warden-hub | **per-user mTLS cert**, authorized by the hub | full / hub-granted scope |

**The key change:** the relay path does **not** require the shared token. A
connection that arrives on the authenticated relay listener is already proven to
be the enrolled owner (mTLS cert from the per-user CA); demanding a second shared
secret on top would be redundant and worse (a shared secret is weaker than a
per-user, revocable, scoped cert). This directly answers the maintainer's point:
*with the hub providing proper authN/authZ, the extra token is not needed for the
hub path.*

**What we keep:** the bearer token remains the authority for **direct LAN/WAN
exposure** — the case where someone binds the raw port on their network without
the hub. That path has no hub identity to lean on, so the token stays its guard.
Loopback stays trust-by-position. So: *remote → hub identity; direct exposure →
token; local → position.* Nothing is removed; the token is **narrowed** to the
one job the hub can't do.

### 6.2 Daemon implementation — one decision point, extended

`authorize()` (`middleware.go`) is already the single auth decision point, and
`authScope` is already an extensible enum. The change is additive:

```go
func (s *Server) authorize(r *http.Request) (bool, authScope) {
    // NEW: a request that arrived on the relay listener carries a verified
    // hub identity (mTLS peer cert + hub-asserted scope). Trust it directly;
    // no bearer token required on this path.
    if id, ok := relayIdentity(r.Context()); ok {
        return true, id.Scope   // scopeFull for owner; scopeReadonly for a read grant
    }
    // ...unchanged: loopback-no-token, then shared bearer token, then readonly.
}
```

The relay connector stamps each stream's `context` with the verified peer
identity + the hub-asserted scope; `authMiddleware`/`isWriteRequest` gating is
untouched (a read grant still can't POST or open a writable `/attach`). No route
changes, no new middleware — just a new *source* of identity feeding the existing
scope machinery.

### 6.3 Authorization granularity (hub side)

The hub can express more than "owner": **grants** let user A give user B (or a
specific device) access to daemon D at a chosen scope. `readonly` maps to the
existing `scopeReadonly`; future scopes (e.g. per-agent, time-boxed) slot into
the same enum. The hub is where these live; the daemon just honors the scope the
verified identity carries.

---

## 7. What changes where (surface summary)

- **Daemon:** new `internal/relay` connector; config block; `warden hub
  register|login|status|connect` CLI verbs; credential storage + rotation;
  `authorize()` relay-identity branch. **No route changes.** All gated on hub
  config (§1.1). ~one contained package + a CLI file + middleware hook.
- **warden-hub:** new private service (accounts, registry, presence, per-user CA,
  rendezvous+relay, grants, abuse controls). The bulk of net-new code (§12).
- **Clients:** `hub://<daemon-id>` target + login/list/connect UX. Web: point at
  the hub; existing fetch/WS/SSE unchanged.
- **Docs (DoD):** README remote-access section, `docs/USAGE.md`,
  `docs/FEATURES.md`, `guides/remote-access.md` (+ a new `guides/warden-hub.md`),
  `reference/cli.md` (gendocs, after the new verbs), skill if agents drive it.

---

## 8. Data model (hub, sketch)

- `users(id, email, auth_provider_id, created_at)`
- `devices(id, user_id, kind[cli|tui|web|android], cert_serial, last_seen)`
- `daemons(id, user_id, name, enrolled_cert_serial, created_at, last_seen,
   online bool)`
- `enrollment_tokens(token_hash, user_id, expires_at, used_at)`
- `grants(daemon_id, grantee_user_id_or_device, scope, expires_at)`
- `ca(user_id, intermediate_cert, crl)`

---

## 9. Presence & lifecycle

- Daemon holds the persistent WSS + heartbeats; hub marks `online` and surfaces
  it in `list my daemons`. Missed heartbeats → `offline` after a grace window.
- Reconnect with capped backoff (mirror the daemon's existing reconnect ethos).
- Client "connect" fails fast with a clear message when the daemon is offline.

---

## 10. Abuse, cost & safety (public relay)

- Per-account: max concurrent tunnels, max relayed bandwidth/day, connection
  rate limits. The daemon's existing `authlimit` per-IP throttle stays for the
  direct path; the hub adds per-account throttles for the relay path.
- The relay is a bandwidth + DoS surface — quotas and backpressure are MVP, not
  later. Metrics + a kill-switch per account.

---

## 11. Hosting model — why a private hub is still trustworthy

Option C makes warden-hub **private and hosted-only** — there is no self-hostable
build. Normally "you must route through my closed server" is a trust ask. The
**E2E design neutralizes it for native clients**: because CLI/TUI/Android run
mTLS end-to-end *through* the hub, the hosted hub relays only ciphertext and
**cannot read agent I/O or inject input**, even though we operate it. A hub
compromise leaks *metadata* (which device talked to which daemon, when), not
session contents. That is the marketing + trust story that replaces
self-hostability:

> "We host the relay, but we are cryptographically blind to your sessions."

The web client is the documented exception (hub-terminated; §1.3) until option-2
(§14) closes it. Users who want zero third-party involvement at all keep the
existing LAN/Tailscale/Cloudflare-Tunnel paths — the hub never removes them.

---

## 12. warden-hub tech stack & infrastructure

**Language: Go — decided.** Not dogma: the daemon's relay client is Go, so a Go
hub **shares the wire protocol, the yamux mux, the WebSocket layer, and the cert
code as common packages** between daemon and hub. One language end to end, and Go
is purpose-built for shuffling bytes across thousands of long-lived connections.

| Concern | Choice | Notes |
|---|---|---|
| **Language** | **Go** | Shares packages with the daemon relay; ideal for a relay |
| **HTTP / routing** | stdlib `net/http` + `chi` | Same as the daemon |
| **WebSocket** | `coder/websocket` | Already a warden dep (daemon attach uses it) |
| **Multiplexing** | `hashicorp/yamux` | Matches the daemon relay side exactly |
| **Database** | **Postgres** (managed: Neon / Supabase / Fly PG) | Multi-tenant accounts/registry/grants want real transactions, concurrent writers, backups. **Not ScrivaDB** — that is the daemon's *embedded local* store, a different tool |
| **Private CA** | **`step-ca`** (smallstep; also Go) | Purpose-built to issue short-lived certs over an API; do not hand-roll X.509 lifecycle |
| **Accounts / authN** | **an auth provider — decided** (Clerk / WorkOS / Supabase Auth; self-host Ory only if needed later) | **Social OAuth — GitHub, GitLab & Google at launch** (decided). Passkeys/MFA fast-follow. Rolling our own accounts is a security-liability time-sink for a solo build. Hub issues its own device certs *after* the provider authenticates the user. **Provider-selection gate: must support all three social logins** — Supabase Auth has GitHub/GitLab/Google built-in; Clerk/others cover GitHub+Google natively and GitLab via generic OIDC/custom OAuth |
| **Dashboard UI** | **Go + `templ` + HTMX — decided** | One language, minimal surface: login, list daemons, generate enrollment tokens, manage grants. No second JS toolchain. Revisit only if the dashboard grows rich |
| **Hosting** | **Fly.io** | Holds long-lived WebSocket connections well, global anycast (good for a relay), cheap Postgres addon. K8s is overkill now |
| **TLS / edge** | platform TLS termination + Let's Encrypt | For the web (hub-terminated) leg and the daemon WSS ingress |
| **Logs / metrics** | `slog` + Prometheus | Same idioms as warden |

Two forks from earlier are now **resolved**: auth = **provider** (not build);
dashboard = **HTMX+templ** (not a JS SPA). Account authN concretely = the
provider's **OAuth-first**, with passkeys as a fast-follow.

---

## 13. Repository, distribution & ownership

### 13.1 Repos

- **warden (daemon + CLI/TUI/web + `internal/relay` connector)** — **open source,
  stays under the personal `srjn45` account.** Decisive reason: it is to be
  claimed as a *personal side project* (e.g. in interviews); org-hosting muddies
  the individual-authorship story and invites commercial/IP questions. The
  relay *client + wire protocol* necessarily live here (the OSS daemon must speak
  the hub protocol) — only the hub *server* is closed. This mirrors the
  Tailscale split (open client, proprietary coordination server).
- **warden-hub (server)** — **new private repo**, personal for now. Move into the
  `spinformati` org **only at commercialization** — GitHub transfers preserve
  full history and authorship, so nothing is lost by deferring.
- **warden-android-app** — **stays public for now** (explicitly parked;
  "problem for another day"). *Future scope if privatized:* private source →
  **Play Store** primary + a **small public releases-only repo** for APK sideload
  (never the main daemon repo — keep the GoReleaser stream clean; a private repo
  cannot serve public downloads; F-Droid needs OSS so it would drop out).

### 13.2 The org / authorship question

- **Git preserves authorship permanently** — every commit is attributed to the
  author regardless of which org/repo hosts it. Moving a repo never erases it.
- **`spinformati`** is the user's GitHub org (not yet a legal entity), intended
  for commercial products sold to small Indian businesses via personal contact
  (billing, inventory management, menu app, restaurant manager), with a planned
  public org web presence. warden's *different intent* (OSS personal-brand tool)
  is why it stays personal while spinformati holds the commercial products. The
  one bridge piece — warden-hub — is warden's commercial arm and is the natural
  eventual spinformati resident, gated on the caveat below.

### 13.3 OSS license note (open question)

The license on OSS warden is a deliberate choice, not a default. Permissive
(MIT/Apache-2.0) maximizes adoption; a copyleft (AGPL) or source-available (BSL)
license is the lever if the goal is to deter a competitor from taking OSS warden
+ building a rival hub and reselling it. Decide before first public tag under the
new domain. (Tracked in §17.)

### 13.4 Domains

- **Buy `srjn45.com`** — personal-brand anchor; directly serves the "clearly my
  own work" intent.
- `warden.srjn45.com` → the site; `warden-hub.srjn45.com` → the hub; optionally
  `relay.srjn45.com` → the raw relay ingress.
- **Domain ≠ repo-owner** — independent decisions. `warden-hub.srjn45.com` can be
  served from a repo under any account/org. `warden-hub.com` is an optional
  defensive/redirect grab, not required.

### 13.5 Employment-IP caveat (NOT legal advice)

The user is employed elsewhere and wants this "separate and clear of legalities."
Before **commercializing** (selling spinformati products, monetizing warden-hub):
read the employment contract for **IP-assignment**, **moonlighting / outside-
employment**, and **non-compete** clauses (often broad in India). Risk is low for
unpaid OSS on personal time/equipment unrelated to the employer's business; it
rises sharply at commercialization — get real legal advice at that point.
Hygiene that strengthens the separation regardless: personal GitHub / email /
machine, personal time only, no reference to the employer. **This caveat gates
the warden-hub → spinformati move.**

---

## 14. Phase-2: option-2 web E2E (flagged, not built here)

To make the **web** client zero-knowledge too, run an application-level handshake
*inside* the browser WebSocket (transport TLS still terminates at the hub, but an
inner Noise/WebCrypto session is browser↔daemon). Requires: (a) a WASM/JS Noise
impl, (b) **daemon-key authentication** so the hub can't MITM the inner handshake
(pin via the per-user CA — this is where zero-knowledge is won or lost), (c)
browser identity key in a non-extractable WebCrypto/IndexedDB store, (d)
rerouting the *entire* web networking layer (fetch, SSE, PTY attach) through the
in-browser tunnel, (e) WASM-crypto perf on the PTY hot path. This is the single
largest sub-project; it is **purely additive** over this build (replaces one
client's transport, changes nothing else). Deferred until web-specific
zero-knowledge is a demonstrated need.

---

## 15. Delivery plan (proposed pipeline)

1. **Spec sign-off** (this doc) + open-questions resolved (§17).
2. **Hub MVP + CLI loop:** accounts (via provider), enrollment, registry,
   presence, rendezvous+relay; `warden hub register/login/connect`; **native E2E
   mTLS** from day one (no trusted-hub phase to migrate off of). Prove: laptop
   CLI → hub → home daemon, full `s.router()` over the tunnel.
2b. Daemon `authorize()` relay-identity branch + config + reconnect (all gated
    on hub config; §1.1).
3. **TUI + Android** onto the same native-E2E path (Android is the primary app
   target; it gets real E2E for free here).
4. **Web** via hub-terminated TLS + grants/authZ dashboard (HTMX).
5. **Abuse controls hardening** + metrics + per-account kill-switch.
6. **(Later)** phase-2 web E2E (§14), if/when needed.

Each stage is a short-lived, reviewable slice — a natural warden pipeline
(design → implement → review per stage).

---

## 16. Security review checklist (before ship)

- Enrollment token: one-time, short-TTL, single-daemon, hashed at rest.
- Daemon-key auth on every E2E handshake (no anonymous relay bridging).
- Per-user CA isolation; revocation actually enforced (CRL/OCSP-lite check).
- Relay authorizes the pairing *before* bridging (owner/grant); no cross-tenant.
- Web-terminated path clearly documented as hub-trusted (not E2E).
- Rate-limit/quota bypass tests; connection-exhaustion tests.
- `hostGuard` + token semantics unchanged for the direct path (regression tests).
- Default local daemon has **no hub connection** unless configured (§1.1 test).

---

## 17. Open questions

1. **Cert lifetime & rotation cadence** for daemon and client certs.
2. **Grants in MVP or v1.1?** (Owner-only is enough to ship; sharing can follow.)
3. **Addressing UX** — `hub://<daemon-id>` vs friendly `hub://<name>` resolution.
4. **Web authZ** — does the hub-terminated web path get full scope by default, or
   should sensitive actions require a native client? (Defense-in-depth question.)
5. **OSS warden license** — permissive (MIT/Apache) vs AGPL/BSL to deter a rival
   reselling a hub on top (§13.3). Decide before the first public tag.
6. **Auth provider pick** — Clerk vs WorkOS vs Supabase Auth. Must clear the
   **GitHub + GitLab + Google** gate (§12); Supabase Auth has all three built-in,
   Clerk needs generic OIDC for GitLab. Final choice is cost/DX within that gate.

*(Resolved: trust model = tiered option-1; hub = private hosted-only; language =
Go; auth = provider with social OAuth (GitHub + GitLab + Google) at launch;
dashboard = HTMX+templ; hosting = Fly.io; warden stays personal/OSS; hub private →
spinformati at commercialization.)*

---

## 18. Definition-of-Done (per CLAUDE.md, at feature completion)

- **Tag & release** — minor bump per feature; confirm version before pushing the
  `v*` tag. (Daemon side only; the hub ships on its own cadence in its own repo.)
- **Docs** — README, `docs/FEATURES.md`, `docs/USAGE.md`, website
  (`guides/remote-access.md` + new `guides/warden-hub.md`, `reference/cli.md` via
  `make gendocs`), skill if agents drive hub flows.
- **CLI help / gendocs** — new `warden hub …` verbs documented; `make gendocs`
  committed (CI-gated).
