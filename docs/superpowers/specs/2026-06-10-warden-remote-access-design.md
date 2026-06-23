# Warden Remote Access — Design Spec

**Date:** 2026-06-10  
**Status:** Future enhancement — high-level design. Detail pass required before implementation.

---

## Summary

Enable warden's web UI to be accessed from any device (phone, tablet, another machine) on a local LAN or over the public internet, without building a relay server. The daemon is the server — the work is auth, network binding, and mobile-responsive UI.

---

## Scope

- **Phase 1 (this spec):** Bearer token auth + configurable bind address + mobile-responsive web UI + documented LAN/WAN setup paths.
- **Phase 2 (deferred):** Native cross-platform mobile app (React Native/Expo). Architecture noted in the Mobile App spec.

---

## Network Modes

### Local LAN

Daemon binds on `0.0.0.0` instead of loopback. Other devices on the same WiFi/network access it at `http://<mac-ip>:PORT`.

Currently controlled by `WARDEN_ALLOW_NONLOOPBACK=1` (boolean). Replace with:
```
WARDEN_BIND_ADDR=0.0.0.0:7979
```
Explicit address + port. Defaults to `127.0.0.1:7979` (loopback-only, safe default). When set to a non-loopback address, auth is automatically required (daemon refuses to start non-loopback without `WARDEN_TOKEN` set).

### Public WAN — No Relay Server Built In

Warden does not run a relay server. Two documented paths:

**Option A: Tailscale (recommended)**
- Install Tailscale on the host machine and the remote device
- Set `WARDEN_BIND_ADDR=0.0.0.0:7979`
- Access via `http://<tailscale-ip>:7979` from anywhere
- End-to-end encrypted, free for personal use, stable IP even when host IP changes
- Tailscale's HTTPS feature (`https://<hostname>.ts.net`) provides TLS — no cert management needed

**Option B: Cloudflare Tunnel**
- `cloudflared tunnel --url http://localhost:7979` → public HTTPS URL
- No VPN, no port forwarding, TLS handled by Cloudflare
- Less persistent URL (changes on restart unless using named tunnel)

Both paths work on Mac and Linux. Warden is agnostic to which is used.

---

## Authentication

**The real new work.** Currently the HTTP API has no auth — safe when bound to loopback, critical gap on any other address.

### Bearer Token

```
WARDEN_TOKEN=<secret>
```

Middleware on every HTTP request: check `Authorization: Bearer <token>`. Return `401` if missing or wrong.

Special cases:
- **Loopback connections** — exempt when `WARDEN_BIND_ADDR` is loopback-only (local CLI/TUI keep working with no config change)
- **WebSocket upgrade** — WS cannot set custom headers; pass token as `?token=<t>` query param on the upgrade request. Middleware reads from query param when header is absent.
- **SSE streams** — same as WS: `?token=<t>` query param.

### Token Generation

```
warden token generate
```

Prints a cryptographically random 32-byte hex string. User sets it in their environment (`export WARDEN_TOKEN=...`) or launchd/systemd unit. No server-side token storage — stateless check.

### Web UI Token Prompt

On first load (or when a request returns 401):
- Modal overlay: "Enter your warden token"
- Stored in `localStorage` under `warden_token`
- Sent as `Authorization: Bearer` header on all subsequent API calls
- Clear token button in settings/header for re-entry

---

## Mobile-Responsive Web UI

Target: usable on a phone browser in portrait orientation (≥320px width).

### Changes

| Area | Change |
|---|---|
| `TabBar` | Collapses to icon-only bottom navigation bar on `<640px` |
| Agent grid | Single column on mobile |
| Cockpit multi-pane | Hidden on mobile; replaced by agent list → tap → full-screen terminal |
| Panels (Metrics, Pipelines) | Stack vertically, charts resize to viewport width |
| `NewAgentModal` / `NewPipelineModal` | Full-screen sheet on mobile |
| xterm.js terminal | Already touch-keyboard compatible; no change needed |

### No TLS in Daemon

TLS is handled by Tailscale or Cloudflare Tunnel at the network layer. The daemon serves plain HTTP. Document: "use Tailscale HTTPS or Cloudflare Tunnel to get TLS on WAN."

---

## Cross-Platform Notes

- Mac: works today after auth + bind-addr change
- Linux: requires Linux support (see Linux spec) before the daemon runs there
- The auth middleware and bind-addr logic are platform-agnostic Go

---

## Security Considerations

- Non-loopback bind without a token set → daemon refuses to start (startup validation)
- Token is a shared secret — document: treat it like a password, use Tailscale ACLs to limit which devices can reach the warden port
- No CORS changes needed for Phase 1 (same-origin web access). Phase 2 (React Native app) will require CORS headers since the app makes cross-origin API calls — add `Access-Control-Allow-Origin` with token validation at that point, not now.
- Rate limiting on auth failures (brute-force protection): lock out an IP after N wrong-token attempts within a window. Optional for v1 since token is mandatory for non-loopback and a 32-byte random secret is infeasible to brute-force; recommended before any public WAN exposure.

---

## Open Questions for Detail Pass

1. Should loopback connections be fully exempt from auth, or require the token everywhere for simplicity? (Exempt = better DX; everywhere = simpler code + no accidental loophole.)
2. Multiple tokens? (E.g., read-only token for mobile viewing vs. full token for spawning.) Probably not v1.
3. ~~Token rotation: `warden token rotate` that updates the env var + restarts daemon? Or just document manual rotation.~~ **Resolved:** `warden token rotate` mints a new token, persists it to `~/.warden/token.env` (chmod 600), and restarts the managed service (systemd restart re-reads the `EnvironmentFile`; macOS rewrites the inlined plist value before kickstart). `warden token show` prints the current token for retrieval. `--no-restart` stages without restarting.
4. Should the web UI persist the token more securely (e.g., `sessionStorage` vs `localStorage`)? `localStorage` persists across tabs/restarts; `sessionStorage` doesn't.
