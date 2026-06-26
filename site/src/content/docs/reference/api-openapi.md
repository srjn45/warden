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

## Authentication

Like `/healthz` and the static SPA shell, the docs page itself is **unauthenticated**
(the spec holds no secrets), but it documents the `bearerAuth` scheme that gates
every data/action route. When the daemon is bound to a non-loopback address it
**requires** a bearer token — send it as `Authorization: Bearer <token>`. Manage the
token with `warden token` and see the [Remote access](/warden/guides/remote-access/)
guide.
