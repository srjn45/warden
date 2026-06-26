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

The spec is derived from the **real routes**: every operation maps to a registered
handler, with schemas modelled off the actual Go types. A CI **drift guard**
(`TestSpecMatchesRoutes`) walks the live router and fails the build if a route is
undocumented or a spec entry is stale — so the reference can't rot.

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
token with `warden token` and see the [Remote access](/warden/guides/remote-access/)
guide.
