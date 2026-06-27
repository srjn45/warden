# warden API response projection — design

**Status:** proposed
**Feature:** token-usage reduction — shrink the JSON warden's own read APIs spill
into an agent's context.
**Surfaces:** read (GET) operations on `/api/v1` (the MCP tools `list_agents`,
`get_agent`, `get_metrics`, `history`, `search`, `show_pipeline`, …) plus a new
`projection` savings category.

## Motivation

warden already measures and shrinks four token sinks: `check` (test/lint spam →
failures-only), `commit` (git tool-spam → compact struct), `llm_offload` (whole
calls served by the local model), and `compact` (context-window reclamation).
Those cover **tool output** and **whole-call offload**.

The sink none of them touch is **warden's own API responses**. Every time an
agent calls a read tool, the *full* JSON lands in that agent's context. The
worst offender is the one agents poll most: `GET /api/v1/sessions` returns a
`Session` with **37 fields** per agent. A coordinator polling a 6-agent fleet
pulls ~220 field-values per `list_agents` call when it usually needs four —
`id`, `name`, `status`, `branch`. Unlike `check`/`commit`, this cost is neither
condensed nor measured.

There is already precedent in the codebase: `getMetricsHistory` carries a
`summary` boolean that returns per-agent summaries instead of raw samples. This
design **generalizes that one-off into a consistent, measured projection lever**
across the read surface.

## Core decision — typed `view` presets, not an untyped `fields=` projector

The spec-first migration (v7.0.0) made response schemas **`x-go-type` aliases to
the real domain types** (`store.Session`, `lifecycle.*Result`, …). That is the
constraint that shapes this design: the Go type behind a response is fixed, so
you cannot simply "drop fields" from an aliased type and still satisfy the
generated `StrictServerInterface`. Two honest options:

- **A. Generic field projection** — a `?fields=id,name,status` query param applied
  by a post-marshal middleware that walks the JSON and keeps the named keys. No
  schema change; works uniformly across every operation.
  - ✅ Zero per-op work, maximal flexibility.
  - ❌ The projection is **invisible to the spec** — it is not in OpenAPI, Swagger
    UI does not show it, and it forfeits the compiler enforcement we just bought.
    An agent can name a field that does not exist and get silent empties. The wire
    contract becomes "whatever JSON keys happen to exist," which is exactly the
    untyped drift the v7.0.0 work eliminated.

- **B. Named `view` presets backed by explicit summary schemas** — a
  `?view=summary|full` query param (default `full`, byte-identical to today). For
  the high-traffic operations, add a real, concrete `SessionSummary` schema (a
  small generated struct: id, name, status, branch, updated_at) and the handler
  maps `store.Session` → `SessionSummary` when `view=summary`.
  - ✅ The lean shape is **in the spec**, compiler-enforced, and shows in Swagger.
    The summary contract is explicit and versioned.
  - ❌ Reintroduces a small hand-written domain→DTO mapping for each projected op —
    the very adapter mapping `x-go-type` removed. But it is compiler-checked and
    confined to the few ops that matter.

**Recommendation: B, scoped narrowly.** The whole point of v7.0.0 was that the
wire format is typed and can't silently drift; a generic untyped projector throws
that away for the sake of breadth we don't need. Start with the one operation
that dominates the cost (`listSessions`/`getSession`), prove the savings, and
extend the preset to the next-worst offenders (`getMetrics`, `listPipelines`,
`listHistory`) only as data justifies. `view` is a closed enum, so an unknown
value is a 400, not a silent empty.

## Spec & codegen changes

1. **New parameter** `View` in `components/parameters` — `name: view, in: query,
   schema: { type: string, enum: [full, summary], default: full }`. Reused by
   reference on each projected operation (same pattern as `SessionId`/`since`).
2. **New schema** `SessionSummary` (concrete — *no* `x-go-type`, so the generator
   emits a real struct). Fields: `id`, `name`, `status`, `branch`, `updated_at`,
   all `required` so the lean shape is byte-stable (per the v7.0.0 byte-stability
   rules).
3. **`listSessions`/`getSession` responses** become a `oneOf`/content-keyed shape
   OR — simpler and strict-friendly — the operation keeps `SessionList` and gains
   a sibling `SessionSummaryList`, selected by the handler. (Open question below:
   strict-server `oneOf` ergonomics vs. two response object variants. Mirror
   whatever `getMetricsHistory` does for its dual `samples`/`summaries` shape so
   the codebase stays consistent.)
4. `make generate` regenerates `api.gen.go`; `*daemon.Server` implements the new
   typed methods. CI `make generate-check` guards drift as usual.

The MCP layer (`internal/mcp`) gains an optional `view` arg on the mapped read
tools, defaulting to `summary` for the fleet-poll tools (`list_agents`) where the
agent almost never needs all 37 fields, and `full` elsewhere — so the default
agent experience gets leaner without a behavior change for code that asks for a
specific session.

## Measuring it — `FeatureProjection` (context axis)

Add `FeatureProjection = "projection"` to `internal/savings`. It falls into the
**context axis** by default (`featureAxis` needs no change — anything not
`llm_offload` is context-shrinking), so it joins the reduction-percentage claim
alongside `check`/`commit`/`compact`, not the offload dollar claim.

Emit one event per projected response, in the daemon handler, the same
fail-open way `recordCheckSavings` does:

- `RawTokens` = `EstimateTokensLen(len(fullJSON))` — what the `full` view would
  have put in context.
- `KeptTokens` = `EstimateTokensLen(len(summaryJSON))` — what the `summary` view
  actually returned.
- `CostTokens` = 0 — projection is a deterministic field-drop; it spends no
  Claude tokens to earn the saving (already net, like `check`).
- `Agent` = the calling session id when known (from the stashed request /
  bearer context), else unattributed.

`savings.NewEvent` derives `Saved = Raw − Kept` and clamps at 0. Provenance
sampling (`savings_samples`) works unchanged — the raw/kept pair is the full vs.
projected JSON, truncated. No new aggregation code: `Summary` already buckets by
feature, so `wd savings` and the dashboard legend pick it up for free (add the
label to the UI legend so it never drifts — see [[features-catalog-structure]]).

## Testing

`internal/daemon` httptest-against-`router()` harness, plus a savings unit test:

- **Default unchanged** — `GET /api/v1/sessions` with no `view` (and `view=full`)
  returns byte-identical output to today (the v7.0.0 byte-stability guarantee).
- **Summary shape** — `view=summary` returns only the `SessionSummary` fields,
  validated against the schema; `view=bogus` → 400.
- **Savings recorded** — a `summary` call appends one `projection` event with
  `Raw > Kept > 0`, `Cost == 0`, attributed to the calling agent; fail-open when
  the savings store is nil/off.
- **MCP default** — `list_agents` defaults to `summary`; a single-session fetch
  defaults to `full`.

## Rollout / Definition-of-Done (per CLAUDE.md)

- Spec edit + `make generate`; handlers; `FeatureProjection`; tests.
- Docs: `docs/FEATURES.md` + root `FEATURES.md` (new savings category and the
  `view` param), `docs/USAGE.md` (the `view` query param + `wd savings` showing
  `projection`), the website (`reference/api-openapi.md` for the param,
  `concepts/` savings page for the new category), and the skill
  (`skills/warden/`) so agents know to pass `view=summary` when polling.
- CLI: `wd savings` legend gains `projection`; check whether any read command
  grows a `--view`/`--full` flag and sync `reference/cli.md` if so.
- One **tag + release** (a `minor` — new measured feature, externally additive),
  confirmed with the maintainer before pushing the `v*` tag.

## Open questions

- **Strict-server dual-response ergonomics.** Cleanest way to express "same
  operation, two response shapes by query param" under oapi-codegen strict mode —
  follow `getMetricsHistory`'s existing dual-array object, or model `view` as a
  content negotiation. Resolve by mirroring the existing pattern.
- **Default view for `list_agents`.** Defaulting the MCP fleet-poll tool to
  `summary` is where most of the saving is, but it is a (minor) behavior change
  for any agent that read a now-omitted field off a *list* result. Audit the
  skill's documented usage first; keep per-session `get_agent` on `full`.
- **Scope creep.** Resist adding `view` to every read op at once. Land sessions,
  measure with the new `projection` ledger, and let the data pick the next op.
