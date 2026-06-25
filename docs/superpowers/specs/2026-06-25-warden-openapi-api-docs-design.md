# warden OpenAPI / API docs — design

**Status:** shipped
**Feature:** FUTURE_ENHANCEMENTS #43 ("Generate `openapi.yaml`; serve Swagger UI at `/api/docs`")
**Surfaces:** `GET /api/docs` (Swagger UI) · `GET /api/docs/openapi.yaml` (raw spec)

## Motivation

warden's daemon already exposes a sizeable REST API — sessions, lifecycle, git
rails, pipelines, snapshots, context/mailbox, metrics — consumed today by the
CLI, the TUI, the MCP server, and the web GUI. Once the daemon is reachable
remotely (bearer-token auth, #13), *outside* consumers appear: a script, a
dashboard, another tool. They need a real reference for the wire format, not a
spelunking trip through `internal/daemon`.

This ships a machine-readable **OpenAPI 3.x** description of every real route plus
an interactive **Swagger UI** at `/api/docs`, served entirely from embedded
assets so it works offline and inside the container image.

## Spec-derived-from-real-routes invariant

The load-bearing constraint: **the spec describes the routes that actually
exist** — no invented endpoints, nothing real left out. The authoritative route
table is `internal/daemon/server.go`'s `router()` plus the `register*Routes`
helpers; `internal/client/client.go` is the second view of the same surface and
was cross-checked path-by-path.

To keep it honest as the API evolves, a **drift guard** (`TestSpecMatchesRoutes`
in `apidocs_routes_test.go`) walks the live chi mux (`chi.Walk`) and asserts a
two-way equality between the concrete routes and the spec's `paths`:

- every registered route (minus the two wildcard routes) has a matching spec path, and
- every spec path corresponds to a real route.

Add an endpoint without documenting it — or leave a stale path in the spec — and
the test fails. The only routes excluded are the wildcards that carry no
documentable schema: the SPA catch-all `/*` and the Swagger UI asset subtree
`/api/docs/swagger-ui/*`.

## Embed / serve approach

Mirrors the existing `web/` embed pattern (`//go:embed` + a small accessor the
daemon reads). A new self-contained package `internal/daemon/apidocs`:

- `//go:embed openapi.yaml` + `//go:embed swagger-ui` — the hand-authored spec and
  a **pinned, vendored** copy of `swagger-ui-dist@5.17.14` (css + the two JS
  bundles + a custom `index.html`). No runtime CDN: the assets are committed and
  embedded, so the UI renders offline and in the container.
- `Spec()` returns the raw document bytes; `SwaggerUI()` returns the asset
  `fs.FS`.

`apidocs_routes.go` registers three routes (before `registerStatic`, so chi's
explicit `/api/docs*` routes win over the SPA catch-all):

- `GET /api/docs` → the embedded Swagger UI page (it points at the spec and loads
  the vendored assets by absolute path, so no trailing-slash redirect dance).
- `GET /api/docs/openapi.yaml` → the raw spec (`application/yaml`).
- `GET /api/docs/swagger-ui/*` → the vendored static assets (path-cleaned,
  traversal-rejected, served from the embedded FS).

The spec itself is a hand-maintained file rather than a code-generated artifact:
warden has no struct-tag→OpenAPI generator in its build, and a small accurate
hand-written spec (guarded against drift by the test above) is lighter than a new
codegen dependency. Schemas are modelled off the real Go types
(`store.Session`/`Event`, the daemon request DTOs, `lifecycle.*Result`,
`snapshot.Snapshot`, `pipeline.Pipeline`, `digest.Digest`, …).

## Auth decision for `/api/docs`

`/api/docs` and the raw `openapi.yaml` are **public** (unauthenticated) — they sit
in the same unauthenticated band as `/healthz` and the static SPA shell, *outside*
the bearer-token group. Rationale:

- The spec describes the API **shape**; it carries no secrets, no data, no
  actions. It's the same trust level as shipping the compiled SPA shell to any
  remote browser.
- Keeping docs public is what lets a remote browser load them and *then* prompt
  for a token — symmetric with how the SPA shell is exposed (FEATURES.md §13).

The spec still documents the `bearerAuth` security scheme and applies it globally
(`security: [{bearerAuth: []}]`), with the three public operations overriding to
`security: []`. So every data/action route correctly shows the token requirement
in Swagger UI; only the health + docs routes are marked open.

## Config gate

Follows the `snapshots` gate pattern exactly: a new `api_docs` config setting
(default **on**) with a schema/description entry and a `GetApiDocs()` accessor.
The daemon wires it via `Server.SetAPIDocs(cfg.ApiDocs)`; when off, all three
docs routes return **404** (the natural answer for a disabled *public* surface —
there's no auth context in which a 403 hint would land). The routes stay
registered either way, so the drift guard's view of the route table is stable.

## Testing

`apidocs_routes_test.go`, using the existing `httptest`-against-`router()` harness
(no live network, no real model):

- **Validity** — the embedded spec parses as YAML, declares `openapi: 3.x`, and
  carries the load-bearing sections (`info`, non-empty `paths`, the `bearerAuth`
  scheme, the `Session`/`Error` schemas, and a representative set of known paths).
- **Drift** — the two-way route↔spec equality described above.
- **Serving** — `/api/docs` returns 200 HTML referencing the spec + assets, the
  raw-spec route serves the document, and a vendored asset is reachable.
- **Gate** — with `api_docs=false` all three routes 404.
- **Public** — with bearer auth enabled the spec route still returns 200 (the docs
  surface is not token-gated).
