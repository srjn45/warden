# agentctl GUI — Design

**Date:** 2026-06-01
**Status:** Approved design (pre-implementation)
**Owner:** Srajan Pathak (personal project)
**Extends:** `docs/specs/2026-06-01-agentctl-design.md` (the GUI was a v1 non-goal, "designed for, not built"; this is that follow-on).

---

## 1. Goal

A local web dashboard, built with **Astro + React**, served by the existing
`agentctl daemon`, that lets me:

- list all agents and see each one's status, live;
- create new agents (any task type);
- manage an agent — send it a message, read its recent output;
- open an agent and see its full history (event log) and what it's doing now;
- have the view monitor in a loop and keep updating automatically;
- see at a glance whether an agent is **busy** or **idle**;
- terminate/clean up an agent, including its worktree/branch.

## 2. Key decisions

| Decision | Choice | Rationale |
|---|---|---|
| Serving | **Static Astro embedded in the daemon** via `go:embed`, served at `/` on `:8765` | Same origin as the API → no CORS, no auth, one binary. Matches the parent design ("static frontend served by the same daemon"). |
| Live updates | **Server-Sent Events** (`GET /events/stream`) | Push, not poll; mirrors the daemon's single-writer model via an in-process broadcaster. |
| Interactivity | **React islands** (`@astrojs/react`), one root island | Familiar; a live SPA-style dashboard is simplest as one stateful root rather than many islands. |
| Live terminal output | **Short-poll `GET /sessions/{id}/output`** while a detail view is open | SSE carries list/status/history/excerpt cheaply; the fuller live pane is fetched only for the one open agent. |
| Dev workflow | Astro dev server (:4321) with **Vite proxy** to :8765 | No CORS in dev; identical same-origin behaviour in prod. |
| New data endpoints | **None** beyond SSE + static | All actions reuse existing REST. |

## 3. Architecture

```
Browser (EventSource + fetch)
        │  same origin :8765
        ▼
agentctl daemon (chi)
├── API routes (unchanged): /healthz /sessions /sessions/{id} /spawn /cleanup
│                           /events /sessions/{id}/input /sessions/{id}/output
├── GET /events/stream      ← NEW: SSE of full session snapshots
├── /*  (catch-all)         ← NEW: serves embedded Astro dist, SPA fallback to index.html
├── hub (broadcaster)       ← NEW: in-process pub/sub; fan-out to SSE subscribers
├── store (Mongo, sole writer)
└── poller (OnChange → hub.publish)   ← NEW callback
```

Route precedence: explicit API routes are registered **before** the `/*`
catch-all, so chi's trie matches them first; `/*` only handles UI/static paths.

### 3.1 Broadcaster (hub)
`internal/daemon/hub.go`:
- `subscribe() (<-chan struct{}, func())` — returns a coalescing channel (cap 1)
  and an unsubscribe func.
- `publish()` — non-blocking send to every subscriber (drops if the cap-1 buffer
  is full, i.e. an update is already pending — coalescing).
- Guarded by a mutex; safe for concurrent publish/subscribe.

**Who publishes:** every mutating HTTP handler after a successful write
(`handleSpawn`, `handleCleanup`, `handleEvent`, `handleInput`) and the poller via
a new optional `OnChange func()` on `poller.Poller`, set to `hub.publish` by the
daemon wiring.

### 3.2 SSE endpoint
`internal/daemon/sse.go` — `GET /events/stream`:
- Headers: `Content-Type: text/event-stream`, `Cache-Control: no-cache`,
  `Connection: keep-alive`. Requires `http.Flusher`.
- On connect: send the current snapshot immediately.
- Subscribe to the hub; on each notification, read `store.List`, marshal to
  JSON, write `data: <json>\n\n`, flush. Skip the write if the snapshot is byte-identical to the last one sent (dedupe).
- Heartbeat: every 25s write a `: ping\n\n` comment to keep the connection alive.
- Exit when `r.Context().Done()` (client disconnect) fires; unsubscribe.

Snapshot payload = the same JSON as `GET /sessions`: `{"sessions":[ ...Session ]}`.

### 3.3 Static embedding
- Astro builds to a `dist/` directory that a Go package can embed.
- `web/embed.go` (`package webui`): `//go:embed all:dist` exposing `Dist fs.FS`
  (sub-rooted at `dist`). A committed placeholder `web/dist/index.html` keeps
  `go build` working before the first UI build.
- `internal/daemon` static handler: serve the requested path from `webui.Dist`
  if it exists; otherwise serve `index.html` (SPA fallback) for GET requests;
  return 404 for non-GET unmatched.

### 3.4 Build & dev
- **Prod:** `make ui` (`cd web && npm ci && npm run build` → `web/dist`) then
  `make build` (go build embeds dist). `build` depends on `ui`.
- **Dev:** run `agentctl daemon` (:8765) + `cd web && npm run dev` (:4321). Astro
  `vite.server.proxy` forwards `/sessions`, `/sessions/*`, `/spawn`, `/cleanup`,
  `/events`, `/events/stream`, `/healthz` → `http://127.0.0.1:8765`.

## 4. Frontend structure (`web/`)

```
web/
├── package.json  astro.config.mjs  tsconfig.json  vitest.config.ts
├── dist/                         build output (gitignored except placeholder index.html)
├── embed.go                      package webui — go:embed all:dist → Dist fs.FS
├── public/                       favicon etc.
└── src/
    ├── pages/index.astro         shell; mounts <Dashboard client:load />
    ├── lib/
    │   ├── types.ts              Session, Event, Status (mirror the Go JSON)
    │   ├── api.ts                listSessions/getSession/spawn/cleanup/sendInput/getOutput + subscribeSessions (EventSource)
    │   ├── status.ts             busyIdle(status) → {label,kind} ; pure, unit-tested
    │   └── status.test.ts        Vitest
    └── components/
        ├── Dashboard.tsx         root island: holds sessions[] (SSE), selection, connection state
        ├── AgentList.tsx         table/cards + BusyIdleBadge + quick terminate
        ├── AgentDetail.tsx       metadata + EventTimeline + live output (poll) + SendBox + TerminateControls
        ├── NewAgentModal.tsx     type-aware create form → spawn
        ├── BusyIdleBadge.tsx     status → colored badge
        ├── EventTimeline.tsx     events[] rendered newest-first
        ├── TerminateControls.tsx cleanup with force/hard + 409 guard handling
        └── api.test.ts           Vitest (URL building, spawn body shape) with mocked fetch
```

### 4.1 Data flow
- `Dashboard` opens `subscribeSessions()` on mount → `EventSource('/events/stream')`;
  each message replaces `sessions[]`. `onerror` sets a "reconnecting…" banner
  (EventSource auto-reconnects). Falls back to a one-shot `listSessions()` fetch
  on mount so the first paint doesn't wait for SSE.
- Selecting a row sets `selectedId`; `AgentDetail` renders from the SSE snapshot
  (status, metadata, `events[]`, `last_pane_excerpt`) and additionally polls
  `getOutput(id)` every 2s for the fuller live pane while mounted.
- Actions (`spawn`, `sendInput`, `cleanup`) are fetches; on success the SSE push
  reflects the change (no manual refetch needed).

### 4.2 Busy/idle mapping (`status.ts`)
| status | badge | kind |
|---|---|---|
| `spawning` | Starting | busy |
| `working` | Busy | busy |
| `waiting_for_input` | Needs input | attention |
| `idle` | Idle | idle |
| `done` | Done | idle |
| `errored` | Error | error |
| `orphaned` | Orphaned | error |

`kind` drives color (busy=green, attention=amber, idle=grey, error=red). The exact
status string is shown alongside the badge.

### 4.3 Create form (NewAgentModal)
Fields: `type` (select, required), `ticket` (optional text), `repo` (text,
required), and conditional — `branch` (development/pr-review), `pr` (pr-review),
`worktree` checkbox (analysis/spike). Submits `POST /spawn`. Errors: 400 → inline
"type and repo required"; 409 → "already exists — open it" with a link to select it.

### 4.4 Terminate (incl. worktree)
"Terminate" → `POST /cleanup {id}`. On 409 (guard: uncommitted/unpushed) show a
confirm dialog explaining the abort, with **Force** (re-POST `force:true` →
removes worktree+branch) and an optional **Hard delete** (`hard:true` → purge doc
instead of archive). On success the row disappears via the next SSE push. Detail
also shows a copyable `agentctl attach <id>` hint (browsers can't attach tmux).

## 5. Error handling

- **SSE drop / daemon down:** `EventSource.onerror` → persistent "Disconnected —
  reconnecting…" banner; auto-reconnects when the daemon returns.
- **spawn 400/409:** inline form messages (see 4.3).
- **cleanup 409 (guard):** Force/Hard dialog (see 4.4).
- **output 404 (session ended):** detail shows "session ended"; list will drop it.
- **fetch failure:** non-blocking toast; the SSE banner covers persistent outages.

## 6. Testing strategy

- **Go (TDD):**
  - `hub`: publish wakes a subscriber; coalesces multiple publishes into one; unsubscribe stops delivery; concurrent publish is race-clean (`-race`).
  - `sse`: `httptest` server — client receives an initial `data:` snapshot, then a new one after `publish()`; heartbeat comment present; disconnect unsubscribes.
  - static routing: `GET /` → 200 `text/html`; `GET /sessions` still JSON (precedence); `GET /nope` → `index.html` fallback; `POST /unknown` → 404.
  - poller `OnChange` fires on a status change.
- **Frontend (Vitest):** `status.ts` mapping table; `api.ts` request URLs and the
  spawn body shape (mocked `fetch`). One Playwright smoke (list renders from a
  stubbed daemon, create flow, terminate-with-force flow) as manual verification.

## 7. Out of scope (this iteration)

- Auth / multi-user (loopback only, as the daemon).
- In-browser tmux attach (terminal-only; we show the command).
- Editing/configuring the daemon, poller interval, or hooks from the UI.
- Mobile-optimized layout (desktop dashboard first).
