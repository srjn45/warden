---
title: REST API & OpenAPI
description: The daemon's HTTP API, the interactive Swagger UI, and the bearer-auth scheme.
---

The daemon exposes a REST API (the same surface the web dashboard, CLI, and MCP
server drive). For programmatic or remote consumers it ships a machine-readable
**OpenAPI 3.x** description and an interactive **Swagger UI**, so you have a real
reference instead of reading the source. Gated by the `api_docs` config setting
(default on).

| Endpoint | What it serves |
|---|---|
| `GET /api/docs` | Interactive **Swagger UI**, served from a pinned, **vendored** copy embedded in the binary — no runtime CDN, so it works offline and inside the container image. |
| `GET /api/docs/openapi.yaml` | The raw OpenAPI document (`application/yaml`). |

The spec is **spec-first**: `openapi.yaml` is the single source of truth, and the
daemon's typed HTTP server is **generated from it** (via `oapi-codegen`). Every
operation becomes a method the server must implement, so an undocumented or
mismatched endpoint is a **compile error** rather than silent drift — and a CI
guard (`make generate-check`) fails the build if the generated code falls out of
sync with the spec. Response schemas alias the actual Go types, so the reference
and the wire format are the same thing by construction.

## Server-sent event stream

`GET /api/v1/events/stream` is an SSE snapshot stream. Its event name is part of
the payload contract:

| SSE event | Payload | Consumer behavior |
|---|---|---|
| *(unnamed/default)* | `{ "sessions": [...], "autopilot"?: ... }` | A complete session snapshot. This is the only event session-list consumers decode. |
| `tree` | The same project-tree object returned by `GET /api/v1/tree`. | Handle through a `tree` listener; it is not a session envelope. |
| `error` | `{ "error": "...", "degraded": true }` | Stream/store failure metadata. Keep the last complete snapshots; reconnect or surface the degraded state. |

Frames are full snapshots, not deltas, and the session and tree frames are
deduplicated independently. A client must ignore unknown named events for forward
compatibility. In particular, it must never decode every `data:` block as a
session list: named events may have unrelated schemas. SSE comments are heartbeats
and carry no payload.

### Validated client semantics

Regression tests (`internal/daemon/sse_test.go`, `internal/client/client_test.go`,
`internal/tui/refresh_test.go`) replay interleaved default/tree/error/malformed
frames across reconnects and pin these rules:

- Only a well-formed unnamed frame produces a snapshot; `tree`, unknown named
  events, comments, and malformed unnamed/named-tree frames never reach session
  callbacks (a malformed unnamed frame is skipped, not treated as an empty fleet).
- A named `error` frame always ends the Go client's watch with a typed
  `*client.StreamError` (`Degraded` set from the payload); a malformed or empty
  error payload still yields a `StreamError`. Frames after it are not delivered.
- The daemon emits a named `error` instead of an empty/partial snapshot while the
  store is degraded, and resumes complete snapshots on recovery.
- The TUI maps degraded stream errors to the degraded fleet state and transport
  drops to timeout/disconnected, retaining the last-known-good fleet. Selection,
  the opened agent pane, and terminal rows survive every error, drop, and
  reconnect, and no terminal is spawned. The next good frame restores live health.

## Base path

Every data/action endpoint is served under a versioned prefix: **`/api/v1`**
(e.g. `GET /api/v1/sessions`, `GET /api/v1/metrics`, `POST /api/v1/spawn`). The
version segment leaves room for a future breaking revision under `/api/v2`
without disturbing existing clients. Only three surfaces live at the **root**:
`GET /healthz` (liveness probe), `GET /api/docs*` (the Swagger UI + spec), and
the static SPA shell. Keeping the API off the root is what lets the dashboard
own bare client-route URLs like `/metrics` and `/pipelines` — a browser
navigation to one of those loads the app, while the JSON lives at
`/api/v1/metrics`, `/api/v1/pipelines`.

> **Breaking change (v5.22+):** the data API moved from the root to `/api/v1`.
> The bundled CLI, MCP server, and web UI were updated in lockstep; only
> third-party scripts that hit the raw HTTP API need their paths prefixed
> (`/sessions` → `/api/v1/sessions`). `/healthz` is unchanged.

## Authentication

Like `/healthz` and the static SPA shell, the docs page itself is **unauthenticated**
(the spec holds no secrets), but it documents the `bearerAuth` scheme that gates
every data/action route. When the daemon is bound to a non-loopback address it
**requires** a bearer token — send it as `Authorization: Bearer <token>`. Manage the
token with `warden daemon token` and see the [Remote access](/warden/guides/remote-access/)
guide.
