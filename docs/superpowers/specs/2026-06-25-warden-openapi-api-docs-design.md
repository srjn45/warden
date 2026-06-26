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

## Spec-first invariant (updated 2026-06-27)

The load-bearing constraint: **the spec describes the routes that actually
exist** — no invented endpoints, nothing real left out. Originally this was an
*invariant maintained by hand*: the spec was authored to mirror `router()` and
the `register*Routes` helpers, with `internal/client/client.go` as a second view
cross-checked path-by-path, and a path-only drift guard
(`TestSpecMatchesRoutes`) walked the live chi mux (`chi.Walk`) asserting two-way
equality between the concrete routes and the spec's `paths`.

That cross-check only validated **path existence** — never request/response
**schemas, parameters, methods, or status codes** — so ~1,800 lines describing
the wire format could silently drift. We have since **flipped to spec-first**:
`openapi.yaml` is now the single source of truth and **`oapi-codegen` generates a
typed ("strict") chi server** from it (`internal/daemon/oapi/api.gen.go`, via
`go generate`). The daemon's `*Server` implements the generated
`StrictServerInterface`, so:

- **Schemas are compiler-enforced.** Every `operationId` becomes an interface
  method whose request/response *types* must match the spec; a missing or extra
  operation, or a mismatched DTO, is now a **build failure**, not a silent gap.
  The 74 hand-written `handleXxx` handlers and the ~40 daemon-local request/
  response DTOs were deleted — the generated types are the only wire surface.
- **The generated file can't drift from the spec.** A CI guard
  (`make generate-check` → `go generate ./... && git diff --exit-code` over
  `internal/daemon/oapi`) fails if the spec changed without regenerating.
- **`TestSpecMatchesRoutes` is downgraded** to a route-presence smoke test: it
  still asserts route↔spec parity for the **hand-registered** routes that sit
  outside strict generation — `/healthz`, the `/api/docs*` surface, the SSE
  stream `/api/v1/events/stream`, and the attach socket — which the generator
  does not own. Those streaming/attach ops are excluded from codegen
  (`config.yaml`'s `exclude-operation-ids`) and registered manually; the SPA
  catch-all `/*` and the Swagger UI asset subtree `/api/docs/swagger-ui/*` carry
  no documentable schema and are skipped as before.

Domain responses reuse the real Go types directly: ~18 schemas carry
`x-go-type` + `x-go-type-import` so the generated model is a **type alias** to
the existing domain type (`store.Session`, `lifecycle.*Result`,
`pipeline.Pipeline`, …). Zero adapter mappings, and the wire output stays
byte-identical to the hand-written handlers it replaced.

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

The spec itself is **hand-authored** (warden has no struct-tag→OpenAPI generator),
but it is no longer just documentation: as of the spec-first flip (above) it is
the **input** to `oapi-codegen`, which generates the daemon's typed server from
it. The direction reversed — code now follows the spec, not the other way round.
Schemas are still modelled on the real Go types (`store.Session`/`Event`,
`lifecycle.*Result`, `snapshot.Snapshot`, `pipeline.Pipeline`, `digest.Digest`,
…), now wired through `x-go-type` aliases so the generated models *are* those
types. The vendored Swagger UI keeps reading the same embedded
`apidocs/openapi.yaml`; the `embedded-spec` the generator can emit is not the
served copy, so there is a single source of truth.

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
- **Drift** — route↔spec parity for the hand-registered routes (the strict
  schemas are enforced by the compiler + the `make generate-check` CI guard).
- **Serving** — `/api/docs` returns 200 HTML referencing the spec + assets, the
  raw-spec route serves the document, and a vendored asset is reachable.
- **Gate** — with `api_docs=false` all three routes 404.
- **Public** — with bearer auth enabled the spec route still returns 200 (the docs
  surface is not token-gated).
