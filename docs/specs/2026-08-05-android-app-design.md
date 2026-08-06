# Warden Android App — Design & Architecture Spec

**Date:** 2026-08-05
**Status:** Planning / design. No code yet. Supersedes the Android portions of
`docs/superpowers/specs/2026-06-10-warden-mobile-app-design.md` (which assumed
cross-platform React Native, excluded spawning, and referenced the pre-ship
`WARDEN_BIND_ADDR`/`7979` config — all outdated).

---

## 1. Goal & MVP scope

warden is mature and offers solid remote access (LAN, Tailscale, Cloudflare
tunnel). The web UI is awkward on phones. This spec plans a **dedicated,
Android-only native app** focused on driving a fleet from a phone.

**MVP features (only these):**

1. **Create / delete agents.**
2. **Open an agent** (view it, its live output).
3. **Interact with an agent** — send prompts, plus an on-screen key bar for keys
   the mobile soft-keyboard lacks (Esc, Tab, arrows, Ctrl-C, `|`, `~`, …).
4. **Raw terminal access** — run arbitrary commands on the host.

The app must let the user **enter the host address + passkey (bearer token)**
manually (with optional QR pairing as a convenience).

**Explicitly out of MVP:** push notifications, pipelines, metrics dashboards,
approvals UI, code editing, iOS, tablet layout. Named here so the first build
stays small and reviewable.

---

## 2. Key insight — the daemon already does the server-side work

Almost nothing new is needed on the warden side. Every MVP feature maps to an
**existing** authenticated `/api/v1` endpoint. Base URL `http://<host>:8765`.

| MVP feature | Endpoint | Protocol |
|---|---|---|
| List agents | `GET /api/v1/sessions` → `{sessions:[…]}` | REST |
| Live fleet updates | `GET /api/v1/events/stream` (full `{sessions:[…]}` snapshots + `:ping` every 25s) | SSE |
| Open one agent | `GET /api/v1/sessions/{id}` | REST |
| Create agent | `POST /api/v1/spawn` (`SpawnRequest`); `428`+`ConfirmationResponse` on memory-pressure gate → retry `force:true` | REST |
| Delete agent | `POST /api/v1/sessions/{id}/terminate` then `POST …/delete` (and `…/remove-worktree` if desired) | REST |
| Send a prompt | `POST /api/v1/sessions/{id}/input` (`InputRequest`) | REST |
| Transcript snapshot | `GET /api/v1/sessions/{id}/output?lines=N` | REST |
| **Interact / special keys / live output** | `GET /api/v1/sessions/{id}/attach` | **WebSocket** |
| **Raw host terminal** | `GET /api/v1/cockpit/attach` (3-pane TUI; master pane is a host shell/REPL) | **WebSocket** |
| Dir picker for spawn cwd | `GET /api/v1/fs/dirs` | REST |
| Backend picker | `GET /api/v1/backends` → `{backends:[{id,display_name,default,available}]}` | REST |

Source of truth: `internal/daemon/apidocs/openapi.yaml`; streaming routes are
hand-registered in `internal/daemon/api.go` (excluded from codegen via
`internal/daemon/oapi/config.yaml`).

### 2.1 The terminal attach protocol (the heart of the app)

`internal/daemon/attach.go` bridges a real PTY (`tmux attach-session`) to a
WebSocket, forcing `TERM=xterm-256color`:

- **server → client:** binary frames = **raw xterm-256color terminal bytes.**
- **client → server, binary frame:** keystrokes written straight to the PTY.
- **client → server, text frame:** JSON `{"cols":N,"rows":M}` resize control.
- Cockpit route emits WS close code `4001` when its tmux session ends.
- **Mouse mode:** the per-agent attach path (`handleAttach`) sets tmux
  `mouse on`, scoped to that session (no `-g`, global config untouched). Agent
  TUIs run in the alt-screen, which has no scrollback, so a forwarded
  mouse-wheel event is the only thing that scrolls their history — the app
  turns a vertical swipe into `MOUSE_WHEELUP/DOWN`, but only when the emulator
  reports mouse tracking active, so it stays a no-op if this is ever off. Being
  a session option, it applies to **every** client of that agent session
  (including the web UI's per-agent terminal): drag-to-select becomes
  Shift-drag-to-select, matching the cockpit/TUI's existing behavior. The
  cockpit path is intentionally left alone — its TUI manages the mouse itself.

This single protocol powers three MVP requirements at once — **agent
interaction, the special-key bar, and raw terminal are all the same widget**: a
terminal emulator fed the byte stream, with a key bar that injects escape
sequences over the socket.

Because tmux holds the session **server-side**, the app can drop the socket when
backgrounded and re-attach later with scrollback intact — no need to fight
Android Doze with a persistent connection.

---

## 3. Auth & networking model (from code, not the old spec)

- **Scheme:** bearer token. `WARDEN_TOKEN` = 64 hex chars from
  `warden token generate`. Stateless constant-time compare; not persisted
  server-side. Optional `WARDEN_READONLY_TOKEN` (GETs + SSE only; 403 on writes
  and on `/attach`).
- **REST:** `Authorization: Bearer <token>`.
- **WS / SSE:** token as `?token=<token>` query param (headers can't be set on
  `EventSource`/WS upgrade).
- **Binding:** config key `addr`, default `127.0.0.1:8765`. Binding a
  non-loopback address **requires** `WARDEN_TOKEN` (daemon refuses otherwise).
- **`hostGuard`:** only enforced when NO token is set (loopback-only, anti-DNS-
  rebinding). Once a token is set it is bypassed — so a properly configured
  remote deployment never trips it.
- **No TLS in the daemon** (plain HTTP). Transport encryption comes from the
  network: **Tailscale (recommended)** or Cloudflare tunnel. No CORS — a non-
  issue for a native client anyway.

**Implication:** the app talks plain `http://` to a Tailscale/LAN IP, or
`https://` to a `*.ts.net` / tunnel domain. It must present the bearer token on
every transport.

---

## 4. Tech stack — Native Kotlin + Jetpack Compose

Chosen because the app is Android-only and terminal-first. (The old spec's
React Native pick optimized for iOS + web-code reuse — neither applies here, and
RN's terminal story, xterm.js in a WebView, is its weakest area.)

| Concern | Choice |
|---|---|
| UI | Jetpack Compose |
| **Terminal widget** | **Termux `terminal-view` + `terminal-emulator`** — battle-tested Android terminal that consumes the raw xterm-256color stream directly; its "extra keys" row is exactly the special-key requirement |
| REST | OkHttp + Retrofit (+ kotlinx.serialization / Moshi) |
| SSE | `okhttp-sse` (EventSource) |
| WebSocket | OkHttp `WebSocket` (binary + text frames) |
| Secure storage | Keystore-backed `EncryptedSharedPreferences` |
| QR pairing (optional) | CameraX + ML Kit barcode |
| Async | Coroutines + Flow |

**Trade-off accepted:** no code reuse with the TS web UI, no free iOS path.
If iOS ever matters, Flutter (`xterm.dart` + `flutter_pty`) is the fallback.

---

## 5. App architecture

```
┌─────────────────────────── Android app ───────────────────────────┐
│  UI (Compose)     Connect · AgentList · CreateAgent · Terminal     │
│  State            ViewModels · ConnectionStore · SessionCache      │
│  Transport  WardenClient                                           │
│    ├─ REST     OkHttp+Retrofit, Bearer interceptor → /api/v1/…      │
│    ├─ SSE      okhttp-sse                           → /events/stream│
│    └─ WS       OkHttp binary WebSocket              → …/attach      │
│  Secure store   EncryptedSharedPreferences (host + token)          │
└────────────────────────────────────────────────────────────────────┘
              │  http(s) over Tailscale / LAN / tunnel
              ▼   warden daemon :8765  (UNCHANGED for MVP)
```

Suggested modules: `:app` (UI), `:data` (WardenClient + DTOs), `:terminal`
(Termux view wrapper + WS bridge + key bar).

### Screens

1. **Connect** — host field (`http://100.x.y.z:8765` or
   `https://box.tailnet.ts.net`) + token field. "Test": `GET /healthz`, then an
   authed `GET /sessions`. Persist to Keystore. Optional "Scan QR"
   (`warden://host:port?token=…`). Support multiple saved hosts.
2. **Agent list** — fed by the SSE stream; status badge, model, age. Pull-to-
   refresh fallback (`GET /sessions`). "+" → create; swipe → delete (confirm).
3. **Create-agent sheet** — backend picker fed by `GET /api/v1/backends` (live
   registry: `id`/`display_name`/`default`/`available`, so the list never drifts
   as warden adds backends and un-installed ones can be greyed out) with a static
   fallback if the endpoint 404s on an older daemon, working dir via
   `GET /api/v1/fs/dirs` browse (no local FS), optional role/model, initial
   prompt. On `428`, show a "spawn anyway" confirm and resend with `force:true`.
4. **Agent detail / terminal** — Termux terminal bound to
   `…/sessions/{id}/attach` + the key bar; a quick-prompt input box that posts
   to `…/input`; a "terminate"/"delete" overflow.
5. **Raw host terminal** — same terminal widget on `…/cockpit/attach`.

### 5.1 On-screen key bar (the "buttons the keyboard lacks")

A horizontally scrollable row above the soft keyboard. Each key writes the
corresponding bytes over the WS. Ctrl/Alt are **sticky modifiers**.

| Key | Bytes sent |
|---|---|
| Esc | `0x1b` |
| Tab | `0x09` |
| Enter | `0x0d` |
| ↑ ↓ → ← | `ESC [ A/B/C/D` |
| Home / End | `ESC [ H` / `ESC [ F` |
| Ctrl-C / Ctrl-D / Ctrl-Z | `0x03` / `0x04` / `0x1a` |
| `Ctrl` (sticky) | next key → control byte (e.g. Ctrl+R → `0x12`) |
| `/  \|  ~  -  esc-shortcuts` | literal ASCII |

This is essentially Termux's extra-keys row, so most of it is a config away.

---

## 6. Gotchas to design around

- **Android cleartext policy:** `http://` to a Tailscale/LAN IP needs a
  `network_security_config` permitting cleartext to user hosts (or restrict the
  app to `https://`). Handle before first LAN test.
- **Reconnect model:** rely on tmux server-side persistence — reconnect + re-
  attach on resume rather than a battery-draining foreground service.
- **Read-only token:** detect via 403 on writes/attach and degrade gracefully.
- **Auth on WS/SSE is query-param only** — never try to set a header there.

---

## 7. Small daemon additions (post-MVP, optional)

None are required for MVP, but worth noting:

- **Dedicated throwaway host shell.** Today the only host-shell path is the
  *shared* cockpit TUI pane. A tiny "spawn a fresh PTY and attach" endpoint would
  give the app a private raw terminal instead of a shared one.
- **Push token registration** (`POST /push-token`) — only if/when notifications
  land (future phase).

---

## 8. Suggested build phases

- **P0 — Connect + list.** Secure host/token entry, `/healthz` + `/sessions`,
  SSE-driven list. Proves auth + transport end to end.
- **P1 — Terminal.** Termux view + WS binary bridge + resize + key bar against
  `…/attach`. The riskiest/most valuable piece; do it early.
- **P2 — Create / delete.** Spawn sheet (backend/dir/role/prompt, `428` handling)
  + terminate/delete.
- **P3 — Raw host terminal** via `…/cockpit/attach`, and quick-prompt `/input`.
- **P4 — Polish.** Multiple saved hosts, QR pairing, reconnect/backoff.

---

## 9. Repo placement

Separate concern from the Go release pipeline. Options: a top-level `android/`
directory in this repo, or a dedicated repo. Recommendation: **separate repo**
(distinct Gradle/Kotlin toolchain; keeps the GoReleaser flow and CI clean). The
daemon API is the stable contract between them; version it against
`openapi.yaml`.
