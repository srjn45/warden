# AI CLI model catalog refresh and safe tier reconciliation

- **Date:** 2026-10-19
- **Status:** Design contract (task `contract-and-inventory`, autopilot plan-71743ebd). No runtime code changes in this PR.
- **Integration branch:** `autopilot/aicli-model-catalog-refresh-and-safe-tier-reconciliation`
- **Audience:** the workers implementing the 8 downstream tasks (§13). They implement this document verbatim; any deviation is recorded as an amendment PR to this file first.

## 0. Problem and goal

warden routes agents to `(aicli, model)` pairs from a **hard-coded seed catalog** (`internal/backendstore/seed.go`). Nothing ever reconciles that catalog with what the operator's account can actually use. The motivating bug: Codex `gpt-5.5` is seeded `tier-1, auto_assign=true`, so the router assigns it to tier-1 work, but the operator's ChatGPT-auth Codex account does not offer it (§2.2 evidence). `wd backend model discover` made it worse. For Codex it reads the **selected default** (`model = …` in `~/.codex/config.toml`) and treats that one value as the available catalog.

Goal: replace stale hard-coded routing with:

1. **Explicit, operator-triggered** model discovery that is account-aware. Nothing runs periodically or in the background.
2. **Explainable** tier, capability and shared-quota-bucket **recommendations**. They are deterministic and rule-based. They are never LLM-classified and never guessed.
3. A **transactional preview → apply → rollback** pipeline over the existing routing rows.
4. ~~Bounded automatic recovery when a launch fails with an invalid model~~ **Deferred** (§9): not delivered. The supported alternative is the manual refresh → apply → `warden switch` workflow.

Non-goals (hard): periodic polling; background LLM classification; automatic paid or generation probes (warden never sends a prompt to "test" a model); guessing unknown provider metadata; collection renames or dual writes (§12); dependence on unmerged PTY02/PTY03 work.

---

## 1. Audit of the merged implementation (baseline `0d531421`)

### 1.1 Surfaces today

| Area | File / symbol | Behaviour today |
|---|---|---|
| Live menu | `internal/cli/models.go` `newModelsCmd` RunE | `warden backend model` (legacy `warden models`) type-asserts `agentbackend.ModelLister` and prints `ListModels()`. Only **cursor** and **antigravity** implement it. |
| Catalog CRUD | `models.go` `list` / `tier` / `add` | Reads and writes `backendstore.ModelEntry` through `openRegistry`: REST when a daemon owns the data dir, otherwise a direct store with the ownership lock (#841). |
| Discover | `internal/cli/modeldiscover.go` | Five hard-coded probes: `cursor-agent --list-models`, `agy models`, `opencode models`, `crush models`, and codex `~/.codex/config.toml` top-level `model`. Errors are swallowed silently. `--import --tier T` inserts every discovered id as `is_custom=true` with the same operator-chosen tier. |
| Adapter interface | `internal/agentbackend/backend.go` `ModelLister{ ListModels() ([]string,bool) }` | Ids only. No account scope, completeness, version, error class or metadata. |
| Usage / account | `internal/backendusage/{claude,codex,cursor,antigravity}.go`, `agentbackend.UsageLimiter` (antigravity) | Already reads **non-secret account/plan** facts: Codex app-server `account/read` + `account/rateLimits/read`; Claude OAuth usage; Cursor `api2.cursor.sh`; agy `retrieveUserQuotaSummary`. Reused as identity and quota evidence (§2). |
| Store | `internal/backendstore/store.go`, `shared.go`; production uses `NewStoreFromDB` (`internal/storage/storage.go:122`) | Collections `backends`, `backend_models` (key `aicli:model`), `backend_roles`, `backend_settings`, `backend_quotas`, `backend_rl_cooldowns`. `upsertModel` rejects a tier that is not `tier-1..3`. |
| Seeds | `seedDefaultsIfEmpty` (runs on **every** store open) | (a) **Deletes** every `is_custom=false` row whose key is not in `DefaultModels()`. (b) **Re-inserts** every missing seed key, which resurrects rows the operator deleted. (c) Backfills `auto_assign` / `quota_scope`. |
| Quota | `backendstore/quota.go`, `capacity/binding.go` | Per-`(aicli, scope)` buckets. `capacity.Resolver` fingerprints `warden-capacity-v1:<aicli>:<profile>`, but production wires `ProfileSource=nil` (`cli/daemon.go:394`), so **every account fingerprints as `default`**. |
| Router | `internal/router/resolver.go` `EvaluateCandidates` / `Resolve` | Per-tier candidates. Rejects disabled, non-auto-assign (unless explicitly preferred), uninstalled, disabled-backend, local, non-subscription (unless `AllowPaid`), and limited candidates. Picks by perishable reset, then headroom, then round-robin. Falls back to adjacent tiers. **No notion of availability.** |
| Spawn | `lifecycle/switch.go` `resolveSpawnTarget` (Spawn `lifecycle.go:1943`, SpawnJob `:3180`) | (a) Exact pin is used as-is (validates the aicli only). (b) A model without an aicli is rejected. (c) An aicli pin resolves via `PreferredBackend`. (d) Otherwise full resolve. If the resolver declines, it falls back to `resolveDefaultModel()`. |
| Hot-swap / handoff | `switch.go` `routerResolve`, `launchSuccessor` | Resolve by tier or role. Refuses when no resolver is wired. |
| Restore | `lifecycle.go:2414` `Restore` | Resumes the recorded `agent.Model`. Re-resolves the capacity binding only. |
| Fork | `SessionForker` (codex) via `SpawnRequest.fork_from` | Uses the source's model unless overridden. |
| Autopilot / brain | `autopilot/controller.go:1120` `selectBrain`, `:1148` `SelectWorkerBackend` | Resolve by role `autopilot` / `worker`. The worker path **discards the ModelID**, so the spawn re-resolves the model for the aicli via path (c). |
| Pipeline / schedule | `pipeline` job spec, `cli/schedule.go` (`model`, `aicli` fields) | Free-text model strings handed to Spawn or SpawnJob. They take the same paths (a)–(d). |
| TUI picker | `tui/cmds.go` `loadSpawnCandidatesCmd` | Builds candidates from `ListModels` + usage snapshot + backends. |
| Web | `web/src` | No model picker today. Only free-text model fields (pipeline spec). |
| API | `apidocs/openapi.yaml` `/api/v1/models` (GET list, POST add), `/models/{backend}/{model}/tier` (PUT), `/backends/rescan`, `/roles/tiers`, `/usage` | MCP `list_models`, `set_model_tier` (`mcp/tools_extra.go`). No discover/refresh endpoint exists. Discover is CLI-only and in-process. |
| Launch failure | poller / lifecycle | **No invalid-model classification or recovery; automatic recovery is deferred (§9)** (searched `invalid model`, `model not found/available/supported`, `does not have access`). |

### 1.2 Defects found (each downstream task cites these ids)

| Id | Defect | Evidence | Owner task |
|---|---|---|---|
| D1 | Codex discover treats the `config.toml` selected model as the catalog, and hard-codes `~/.codex` (ignores `CODEX_HOME`). | `modeldiscover.go` `discoverCodexConfigPath`, `parseCodexConfigModels` | provider-discovery-adapters |
| D2 | `Antigravity.ListModels` (`parseAgyModels`) returns whole lines. agy 1.4.2 prints `id<TAB>Display`, so ids come back as `"gemini-4-argon-high\tGemini 4 Argon (High)"`. The discover parser handles tabs. Two divergent parsers. | `antigravity.go:844-880` vs `modeldiscover.go parseAgyDiscover`; live `agy models` output | provider-discovery-adapters |
| D3 | Seeds are stale against live accounts. Codex seeds `gpt-5.5` (T1, auto), `gpt-5.4`, `gpt-5.4-mini`: none are in the account cache. Antigravity: only `gpt-oss-120b-medium` of 14 seeded ids is still listed. Cursor: about 52 live ids are unseeded. | §2.2 | tier-and-quota-policy, rollout |
| D4 | `seedDefaultsIfEmpty` resurrects operator-deleted seed rows on every open. It prunes by seed membership only, so a discovered `is_custom=false` row would be deleted by any binary. | `store.go:861-` | catalog-snapshots-and-policy |
| D5 | Path (c) and (d) fallback `resolveDefaultModel()` returns the **Claude** default (`claude-sonnet-4-6`) even when the pinned aicli is codex/cursor/…, and `launchModel` passes it raw (e.g. `codex -m claude-sonnet-4-6`). Plausible from code; add a regression test before fixing. | `switch.go:412,426`; `lifecycle.go:584` | bounded-startup-failure-recovery |
| D6 | No account scoping: `ProfileSource=nil` in production. | `cli/daemon.go:394` | catalog-snapshots-and-policy |
| D7 | No invalid-model launch detection or recovery. | §1.1 | **Deferred** (§9): not built; manual workflow only |
| D8 | The crush `(not configured)` filter is dead on crush v0.80.0 (0 markers in 1813 rows), so `discover --import` would import the whole public provider registry. | live `crush models` | provider-discovery-adapters |
| D9 | Discover errors are swallowed, so an operator cannot tell "no models" from "not signed in". | `discoverInstalledModels` | operator-api-cli-and-surfaces |

---

## 2. Adapter support matrix

### 2.1 Method

Verified on this host on 2026-10-19. Only these were run: `--version`, `--help`, and non-generating list or status commands (`cursor-agent --list-models|about|status`, `agy models`, `opencode models`, `crush models`, `claude auth status --json`, `codex debug models --bundled`, `goose info`). Local cache files were read with sensitive values redacted. Nothing was run that sends a prompt, generates tokens, logs in, or refreshes credentials. **A public or provider catalog is never taken as proof of subscription CLI entitlement.** Rows marked "verify" must be re-checked by provider-discovery-adapters against the installed binary before shipping, still under the same no-generation rule.

### 2.2 Matrix

Legend for **completeness**: **C** = complete for the account scope (eligible to retire models, §6). **P** = partial or catalog-only (never retires). **—** = no discovery (manual).

| aicli (binary, version seen) | Discovery source(s), in precedence order | Account / profile identity (non-secret) | Auth / billing modes | Cache location and freshness | Completeness | Model / reasoning capabilities | Quota evidence |
|---|---|---|---|---|---|---|---|
| **claude** (`claude` 2.1.303) | **None.** There is no list command. `--model` takes an alias (`fable`/`opus`/`sonnet`/`haiku`) or a full name, and the CLI resolves aliases itself. | `claude auth status --json` (local) → `authMethod` (`claude.ai`), `apiProvider` (`firstParty`), `orgId`, `subscriptionType`. `email` and `orgName` are **never stored**. | claude.ai subscription; API key / Bedrock / Vertex via `apiProvider` | none | **—** (manual). Seed aliases stay `unknown`. | Aliases only. Reasoning is in-session, not a model id. | `backendusage/claude.go` OAuth usage (`five_hour`), bucket `session` |
| **codex** (`codex-cli` 0.160.1) | 1. `$CODEX_HOME/models_cache.json` (default `~/.codex`), written by the CLI for the signed-in identity. 2. `codex debug models` ("render the raw model catalog", may refresh it: metadata only; **verify** no generation and record the network call). 3. Candidate: app-server `model/list` over the same stdio JSON-RPC the usage adapter already uses (**verify** with `codex app-server generate-json-schema`). **Never a source:** `codex debug models --bundled` (binary-shipped; it lists `gpt-5.5`, which the account does not get) or `config.toml model` (the selected default only). | Cache `identity` (opaque provider hash). `auth.json.auth_mode` (`chatgpt` \| `apikey`; tokens never read). App-server `account/read` → `type`, `planType`. `CODEX_HOME` path. | ChatGPT plan (subscription) or API key (pay-per-use) | `models_cache.json`: `fetched_at`, `etag`, `client_version`, `identity`, `models[]`. Fresh when `fetched_at` ≤ 24h ago, `client_version` == installed version, and `identity` == the identity of the current auth. | **C** only for the cache, when fresh, matching, and `visibility` is honoured. `debug models` output is C when it reports the account identity (verify), otherwise P. | `slug`, `display_name`, `visibility` (`list`\|`hide`), `supported_in_api`, `priority`, `supported_reasoning_levels[].effort`, `default_reasoning_level`, `context_window`, `service_tiers`, `upgrade` | app-server `account/rateLimits/read`, bucket `codex` |
| **cursor** (`cursor-agent` 2026.10.01) | `cursor-agent --list-models` (== `models`, "List available models for this account"). Header `Available models`, rows `id - Display [(current, default)]`, `Tip:` footer. 265 rows seen. | `about --format json` → `subscriptionTier`, `cliVersion`, current `model`. `userEmail` is **hashed, never stored**. `status --format json` for login state. | Cursor plan (Pro …): included vs API-priced routes | none (live) | **C** when exit 0, the header is seen, and ≥1 row parses. | Reasoning and speed are encoded in ids (`-low/-high/-xhigh/-fast`). `--help` notes **parameterized** ids with bracket params. These are listed verbatim, never synthesized. | `backendusage/cursor.go`; buckets `included` / `api` / `auto` (`cursorQuotaScope`) |
| **antigravity** (`agy` 1.4.2) | `agy models`: rows `id<TAB>Display`; `Fetching available models...` goes to stderr (drop it on either stream). 13 rows seen. | `$AGY_HOME/antigravity-oauth-token` `auth_method` (tokens never read for this purpose) + the quota RPC plan label. | Google consumer (free tier) / workspace | none (live) | **C** when exit 0 and ≥1 tab row parses. Display-only rows (legacy format) → **P**. | Effort is encoded in ids (`-low/-medium/-high`). | `FetchUsage` → buckets `gemini` / `non-gemini` (5h + weekly) |
| **opencode** (`opencode` 1.17.11) | `opencode models [provider]`: configured providers only (11 rows seen). `--verbose` adds metadata. **Never** `--refresh` (network) and **never** `~/.cache/opencode/models.json` (the models.dev public catalog). | Configured provider ids (from the `provider/` prefix). No account. | Per-provider (free / BYO key / local) | models.dev cache: metadata enrichment only | **P** (catalog of configured providers, not entitlement) | Per `--verbose` (costs, limits): enrichment only | none |
| **crush** (`crush` v0.80.0) | `crush models [search]`: the **whole provider registry** (1813 rows). The `(not configured)` marker was absent in practice. | `~/.config/crush/crush.json` configured provider ids (names only) | BYO key / local | `~/.local/share/crush/providers.json`: registry | **P**. Rows whose provider is unconfigured → `unsupported`, and they are never proposed for add. | none | none |
| **aider** (`aider` 0.86.2) | **None.** `--list-models <substr>` is the litellm public registry, not entitlement. | none | BYO key | none | **—** (manual) | none | none |
| **goose** (`goose` 1.39.0) | **None.** `goose session` has no `--model`; the model comes from `config.yaml` `GOOSE_PROVIDER`/`GOOSE_MODEL` (a selected-model hint only). | none | provider-configured | `goose info` paths | **—** (unsupported: warden cannot select a model) | n/a | none |

Every source has a per-aicli hard timeout of **20s**. Its stdout is capped at **4 MiB**; output beyond the cap is a `partial_result`.

### 2.3 Evidence snapshot (2026-10-19, this host; ids only)

- Codex account cache (`client_version 0.160.1`, `fetched_at 2026-10-19T17:08Z`) has 9 slugs: listed `gpt-6.1-sol, gpt-6-astra, gpt-6-sol, gpt-6-luna, gpt-5.6-sol, gpt-5.6-terra, gpt-5.6-luna`; hidden `gpt-reserve, codex-auto-review`. `config.toml model = gpt-5.6-terra`. The bundled catalog (11) additionally lists `gpt-5.5` and `gpt-daybreak-*`, which confirms that bundled ≠ entitlement. Seeded `gpt-5.5` / `gpt-5.4` / `gpt-5.4-mini` are absent from the account.
- agy: `gemini-4-argon-{low,medium,high}`, `gemini-3.8-flash-{low,medium,high}`, `claude-opus-5-5-{low,medium,high}`, `claude-sonnet-5-5-{low,medium,high}`, `gpt-oss-120b-medium`.
- Tests use recorded fixtures under `internal/agentbackend/backends/testdata/modelcatalog/<aicli>/<version>/`, with account identifiers replaced by `REDACTED`.

---

## 3. Adapter interface (additive)

`ModelLister` stays as-is for one compatibility window. It is implemented as a projection of the new interface. Add to `internal/agentbackend/backend.go`:

```go
// ModelCatalogSource is an optional Backend extension for operator-triggered,
// non-generating model discovery. Implementations MUST NOT send prompts,
// start sessions, log in, refresh credentials, or read secret token values.
type ModelCatalogSource interface {
    // ResolveScope reads local, non-secret identity facts. ok=false => scope_unresolved.
    ResolveScope(ctx context.Context) (CatalogScope, error)
    // DiscoverModels returns one observation set for scope. It never returns a
    // nil Observation; failures are expressed via Completeness + Err.
    DiscoverModels(ctx context.Context, scope CatalogScope) CatalogObservation
}

type CatalogScope struct {
    AiCli        string // registry id
    AuthMode     string // "chatgpt"|"apikey"|"claude.ai"|"firstParty"|"google"|"provider-config"|"" (unknown)
    AccountRef   string // raw non-secret provider identity (e.g. codex cache identity, claude orgId); hashed before persistence, never logged
    Profile      string // CODEX_HOME / config dir / profile name; hashed before persistence
    PlanLabel    string // display-safe ("Pro", "pro", "Free Tier"); stored verbatim
    CLIVersion   string
}

type CatalogObservation struct {
    Source         SourceKind   // live_command|cli_cache|provider_catalog|bundled|config_hint|launch_rejection
    SourceDetail   string       // e.g. "cursor-agent --list-models", "$CODEX_HOME/models_cache.json"
    SourceFetchedAt time.Time   // cache fetched_at; zero for live
    Completeness   Completeness // complete|partial|empty|failed
    Err            *CatalogError
    Models         []ObservedModel
}

type ObservedModel struct {
    ModelID          string   // exact id --model/-m accepts
    DisplayName      string
    Visibility       string   // "listed"|"hidden" (codex visibility=hide)
    ReasoningLevels  []string // provider-declared; empty = unknown, never inferred
    DefaultReasoning string
    ContextWindow    int      // 0 = unknown
    UpgradeTo        string   // provider-declared successor id; "" = none
    Raw              map[string]string // small allowlisted provider facts (supported_in_api, priority, service_tiers ids)
}
```

Rules:

- The `AccountRef` and `Profile` hashes are `ScopeFP = "sha256:" + hex(sha256("warden-model-scope-v1:" + aicli + ":" + authMode + ":" + accountRef + ":" + profile))[:24]`. The capacity fingerprint keeps its own prefix. catalog-snapshots-and-policy wires `capacity.ProfileSource` from `ResolveScope().Profile`, which closes D6 without changing existing bindings: blank profile → `default`, as today.
- Adapters with no discovery do not implement the interface. The service reports `unsupported` for them; adapters are never asked to fake it.
- **CLI behaviour** (D1): codex resolves `CODEX_HOME` the same way `codexHome()` does (`codex.go:398`). `config.toml model` is only ever emitted as a separate `config_hint` observation (`Completeness=partial`), which marks the operator's selected model in previews.

---

## 4. CLI, API and MCP contract

### 4.1 Naming

Canonical: **`wd aicli models refresh [--aicli ID] [--apply] [--json]`** (`warden aicli models refresh`).

- `aicli` is a new namespace node added by this feature (help group `observe`). It contains `models refresh`, `models history`, `models rollback`, and a read-only `models show`. The pending rename series (§12) will move the remaining `backend …` commands under it later. This feature must not move them.
- Retained aliases, all running the same RunE: `warden backend model refresh`, `warden models refresh`. `--backend ID` is a hidden deprecated alias of `--aicli`. If both are given with different values → usage error `alias_conflict`, exit 2.
- `warden backend model discover` stays for one compatibility window as a **preview-only alias**. It runs refresh without `--apply` and prints the old `DiscoveredModel` shape under `--json`. `discover --import --tier T` is deprecated. It runs refresh with `--apply`, restricted to **add** ops, with `tier=T` recorded as operator intent. It prints a deprecation notice on stderr.
- `warden backend model` with no subcommand (the live menu) is unchanged in shape. It is reimplemented over `DiscoverModels`, which fixes D2.

### 4.2 Commands

| Command | Effect | Exit codes |
|---|---|---|
| `aicli models refresh [--aicli ID]…` | Preview. Runs discovery (§5) for the selected aiclis (repeatable flag; default = every installed, enabled aicli). Persists snapshots and observations. Builds and stores a Proposal. Prints the diff. **Never changes routing rows.** | 0 = proposal computed (even if empty); 3 = every selected aicli failed discovery; 2 = usage |
| `… refresh --apply` | Same as preview, then applies that proposal in the same command (§7). Blocked ops are reported, never applied. | 0 applied (or nothing to apply); 4 = `proposal_stale` / `revision_conflict` (re-run); 3 / 2 as above |
| `aicli models history [--aicli ID] [--limit N]` | Lists revisions newest-first. | 0 |
| `aicli models rollback [REV] [--aicli ID]` | Rolls back revision `REV` (default: head) as a new forward revision (§7.3). | 0; 4 = drift conflict (lists keys) |
| `aicli models show [--aicli ID] [--json]` | Per-model view: intent / observed / recommendation side by side. | 0 |

`--json` output is the REST response body verbatim (§4.3). Human output is a table with these columns: `OP AICLI MODEL FIELD BEFORE→AFTER REASON`, then a `BLOCKED` section, then per-aicli `SOURCE COMPLETENESS SCOPE(plan,auth) AGE ERROR`.

Routing: like every registry command, it goes through `openRegistry`. When a daemon owns the data dir it calls REST; otherwise it opens the store directly with the ownership lock. Discovery I/O runs **in the calling process for direct mode** and **in the daemon for REST mode**. In both cases it runs before any store lock is taken (§11).

### 4.3 REST (spec-first: edit `internal/daemon/apidocs/openapi.yaml`, then `make generate`; never hand-write handlers or DTOs)

| Method / path | operationId | Body → response |
|---|---|---|
| `POST /api/v1/aicli/models/refresh` | `refreshModelCatalog` | `{aicli?: string[], apply?: bool}` → `RefreshResult {proposal: Proposal, sources: SourceReport[], applied_revision?: Revision}` |
| `POST /api/v1/aicli/models/proposals/{id}/apply` | `applyModelProposal` | `{}` → `Revision`; 409 `proposal_stale`/`revision_conflict`; 410 `proposal_expired` |
| `GET /api/v1/aicli/models/revisions` | `listModelRevisions` | `?aicli=&limit=` → `Revision[]` |
| `POST /api/v1/aicli/models/revisions/{rev}/rollback` | `rollbackModelRevision` | `{}` → `Revision`; 409 with `conflicts[]` |
| `GET /api/v1/aicli/models/catalog` | `getModelCatalog` | `?aicli=` → `CatalogView[]` (intent + observation + recommendation) |

These are not streaming routes, so no `exclude-operation-ids` change is needed. The existing `/api/v1/models*` routes are unchanged. `ModelEntry` gains only the additive fields of §5.2, all `omitempty`.

MCP (in `internal/mcp/tools_extra.go`, CLI/MCP parity): `refresh_model_catalog {aicli?, apply?}`, `list_model_revisions`, `rollback_model_revision {rev}`. Refresh with `apply=true` is an operator-intent action. An autopilot or brain agent **must not** call it without an operator instruction. This is written into the tool description and the `skills/warden` skill. It is not enforced in code, because warden has no actor-type gate for registry tools today.

### 4.4 Error taxonomy

Every source failure produces a `CatalogError {code, message, retryable}`. **No error class is ever evidence for retirement.**

| code | Meaning | retryable | Effect on availability |
|---|---|---|---|
| `not_installed` | binary not on PATH | no | aicli → `unsupported` for this run; rows untouched |
| `version_unsupported` | parser has no fixture for this major version or the output shape is unknown | no | no change |
| `unsupported` | adapter has no `ModelCatalogSource` | no | rows → `unsupported` (display only) |
| `unauthenticated` / `auth_expired` | CLI not signed in | no | no change |
| `scope_unresolved` | identity could not be read | no | snapshot stored as `partial`; no change |
| `network` / `timeout` | I/O failure or 20s deadline | yes (1 retry) | no change; previously-available rows may age to `stale` |
| `rate_limited` | provider throttled the metadata call | no (no retry) | no change |
| `parse_error` | output did not match the format | no | no change |
| `empty_result` | parsed OK with zero models | no | `completeness=empty`: no change |
| `partial_result` | output truncated, or some rows failed to parse | no | add/update only; no retirement |
| `cache_missing` / `cache_stale` / `cache_identity_mismatch` | codex cache absent, older than 24h, or for another identity | no | `stale` for previously-seen rows; never `unavailable` |
| `proposal_stale` | head revision or row pre-image changed since the preview | — | re-run refresh |
| `proposal_expired` | proposal older than 15 min | — | re-run refresh |
| `revision_conflict` | rollback drift (§7.3) | — | manual resolution |
| `store_unavailable` | registry or DB error | yes | nothing written |
| `alias_conflict` | `--aicli` and `--backend` disagree | — | usage error |

Retry limits: one retry per source, only on `network`/`timeout`, with 2s backoff. One refresh has a shared deadline of **60s** across all aiclis. Sources run concurrently, bounded to **4** in flight.

### 4.5 Explicit-pin semantics

- **Spawn pin** (`--aicli X --model Y`, a pipeline job, a schedule, `fork_from` with a model): the pin is launched exactly as given. It is never re-routed, never substituted, and never blocked by catalog state. If the active-scope observation is `unavailable` or `launch_rejected`, the spawn proceeds and records an `model_catalog_warning` event, and the CLI prints a warning on stderr. A pinned launch that fails as invalid-model is reported, **not** recovered to another model (automatic recovery is deferred, §9).
- **Operator intent pin** (a field set via `models tier`, `models add`, `PUT /models/{b}/{m}/tier`, enable/disable, `refresh --apply` of a recommendation the operator accepted): the field name is appended to the row's `operator_set` (§5.2). Reconciliation **never proposes changing an `operator_set` field**. Retirement is the single exception (§6.2), and its effect is reversible: the prior value is recorded and restored on unretire.
- `PreferredBackend` without a model (path c) never selects a `retired` row. It may select `unknown`/`stale` rows (§8).

---

## 5. Schemas

### 5.1 Identity

`ModelIdentity = (aicli, scope_fp, model_id)`.

- `aicli` is the registry id. Persisted field names follow the current authority (`backend_id` in `backend_models`, `ai_cli` in new records). New collections use the canonical `ai_cli` field name, aligned with `QuotaBinding.ai_cli` and the rename series contract.
- `scope_fp` is §3's `ScopeFP`. `""` = an unscoped aicli (no discovery).
- `model_id` is the exact id the CLI accepts on `--model` / `-m`.
- **Routing rows stay keyed `aicli:model`** (current authority, unchanged). Observations are per scope. Routing reads the observation for the aicli's **active scope** (§5.4).

### 5.2 User intent: `backend_models` (existing collection, additive fields only)

```jsonc
{
  "backend_id": "codex", "model_id": "gpt-5.6-terra",           // existing
  "tier": "tier-2", "display_name": "…", "enabled": true,        // existing
  "auto_assign": true, "is_custom": true, "quota_scope": "codex",// existing
  // --- additive (omitempty) ---
  "origin": "seed|custom|discovered",          // absent => "custom" if is_custom else "seed"
  "operator_set": ["tier","auto_assign"],     // fields the operator set explicitly
  "lifecycle": "active|retired|excluded",     // absent => "active"
  "retired_at": "RFC3339", "retired_by_rev": 12,
  "retired_prior": {"auto_assign": true},     // restored on unretire
  "catalog_rev": 12                           // last revision that wrote this row
}
```

- A row created by discovery is written with **`is_custom=true` and `origin=discovered`**. This way no binary, old or new, prunes it as an unknown seed.
- `tier` gains the value **`unclassified`**: `backendstore.TierUnclassified`. `ModelTier.Valid()` stays tier-1..3 for routing. `upsertModel` accepts `unclassified` through a new `ValidStored()`. `ListModels(tier)` never returns `unclassified` rows for a tier filter, so they are never routed. `models tier` may set it.
- `lifecycle=excluded` is the tombstone written when the operator deletes a **non-custom** row (`DeleteModel`): `enabled=false, auto_assign=false`. Custom rows are still hard-deleted.
- `lifecycle=retired` sets `auto_assign=false` in the row itself (and records `retired_prior`), so an **old binary** that ignores `lifecycle` still never auto-routes it.

### 5.3 Observed availability

New collection **`model_catalog_observations`**. Key `ai_cli|scope_fp|model_id`, using `|` as the separator because model ids may contain `:` and `/`. Keys longer than 128 bytes (the `dbtx.MaxKeyLen` limit) use `ai_cli|scope_fp|sha256(model_id)[:32]`, and the full id is kept in the record.

```jsonc
{ "ai_cli":"codex","scope_fp":"sha256:…","model_id":"gpt-5.5",
  "availability":"available|unavailable|unknown|stale|unsupported",
  "visibility":"listed|hidden",
  "last_seen_at":"…","last_checked_at":"…","evidence_snapshot":"snap-…",
  "launch_rejected_until":"…","launch_rejection_reason":"…",   // observation field; set only by launch evidence, never by automatic recovery
  "capabilities":{"reasoning_levels":["low","medium"],"default_reasoning":"medium","context_window":272000,"upgrade_to":""} }
```

### 5.4 Snapshots, scopes, proposals, revisions (new collections)

All new collections are owned by `backendstore` and registered through `CollectionSpecs()` on the shared handle at `Version 1`. They are deliberately **outside** the rename mapping (§12).

| Collection | Key | Record |
|---|---|---|
| `model_catalog_scopes` | `ai_cli` | `{ai_cli, active_scope_fp, auth_mode, plan_label, cli_version, resolved_at}`: the scope the last successful refresh resolved; used by routing |
| `model_catalog_snapshots` | `snap-<ULID>` | `{id, ai_cli, scope_fp, auth_mode, plan_label, cli_version, source, source_detail, source_fetched_at, observed_at, completeness, error{code,message}, models: ObservedModel[], content_hash}` |
| `model_catalog_proposals` | `prop-<ULID>` | §7.1 |
| `model_catalog_revisions` | `rev-<%020d>` + singleton `__head__` `{rev}` | §7.2 |

**Retention.** Run at the end of every refresh, in the same apply transaction or a separate one. Never periodic.

- Snapshots: keep the newest 20 per `(ai_cli, scope_fp)`, plus any referenced by a retained revision or an open proposal. Delete the rest after 30 days.
- Proposals: `open` expire after 15 min. Terminal proposals are kept 7 days.
- Revisions: the newest 100, or 180 days, whichever keeps more. Never delete head or its parent.
- Observations: no TTL. An observation for a scope that has not been active for 90 days is pruned.

---

## 6. Availability and lifecycle transitions

### 6.1 Observation transitions (per model, on a new snapshot S for scope σ)

Freshness: `stale_after = 24h` (and codex cache `fetched_at` ≤ 24h).

| Prior \ S | S complete, fresh, model present | S complete, fresh, model absent | S partial / empty / failed (any error) | S cache-only stale or identity mismatch | aicli has no source |
|---|---|---|---|---|---|
| (none) | available | — (no row created) | unknown (only for config_hint / seed rows) | unknown | unsupported |
| unknown | available | **unavailable** | unknown | unknown | unsupported |
| available | available (refresh `last_seen_at`) | **unavailable** | available → **stale** once `now - last_seen_at > 24h`, otherwise unchanged | stale | unsupported |
| stale | available | **unavailable** | stale | stale | unsupported |
| unavailable | available | unavailable | unavailable | unavailable | unsupported |
| unsupported | available | unavailable | unsupported | unsupported | unsupported |

`hidden` visibility (codex `visibility=hide`) counts as **present** (available) but is never proposed for `auto_assign` (§8).

### 6.2 Completeness-gated retirement

A **retire** op for row `aicli:model` is proposed only if **all** of the following hold. Otherwise it is listed under `blocked` with a reason:

1. The latest snapshot for the aicli's active scope σ has `completeness=complete` and no `err`.
2. That snapshot is fresh: `observed_at` ≤ 24h at apply time, and `source_fetched_at` ≤ 24h for cache sources.
3. σ == `model_catalog_scopes.active_scope_fp`, and σ ≠ `""`.
4. The observation for this model under σ is `unavailable`.
5. Row `lifecycle=active`.
6. Mass-retirement guard: the op is blocked (`mass_retirement_guard`) if it would retire more than **50%** of the aicli's currently `active`, `available|unknown|stale` rows. The operator can still retire manually by setting the row `enabled=false`.

Absence never retires when it comes from partial output, an empty result, auth, parse, network, timeout or rate-limit errors, cache-only or stale data, a public or bundled catalog, `config_hint`, or launch rejection.

**Unretire.** Proposed automatically when a retired row's observation becomes `available`. It restores `retired_prior` and sets `lifecycle=active`.

### 6.3 Seed vs snapshot authority

| Situation | Rule |
|---|---|
| Seed key absent from the store | Insert with `origin=seed`, `lifecycle=active` (as today). |
| Seed key present with `lifecycle=excluded` or `retired` | **Do not touch.** A seed never resurrects excluded or retired rows (fixes D4). |
| Prune | Delete only rows where `origin=seed`, the key is not in the current seed list, `operator_set` is empty, and no observation for any scope is `available`. Rows with `origin=discovered`, `custom`, or any `operator_set` field are never pruned. |
| Backfill (`auto_assign` / `quota_scope`) | Only when the field is absent **and** not in `operator_set`, as today. A seed never sets `auto_assign=true` on a row whose active-scope observation is `unavailable`. |
| Seed tier vs discovery | The seed is a **recommendation input** (§8). It never changes availability, and never overrides `operator_set`. |

### 6.4 Compatibility, upgrade and rollback matrix

| Scenario | Behaviour |
|---|---|
| Upgrade: existing store, no refresh ever run | No observations → every row is `unknown` → routing is identical to today. Seed changes in this release land through the D4-fixed seeder. |
| Upgrade: new seeds drop stale ids (e.g. codex `gpt-5.5`) | Existing seed rows not in the new list are pruned per §6.3. Rows with `operator_set` survive. Release notes tell operators to run `wd aicli models refresh`. |
| Downgrade to a pre-feature binary | Old code ignores the additive fields and the new collections. Discovered rows survive (`is_custom=true`). Retired rows stay non-auto-assigned (`auto_assign=false` in the row). Excluded tombstones are `enabled=false`. The old seeder may re-insert a seed key only if the row was hard-deleted, which this feature never does for seed rows. Old binaries can write `ModelEntry` without the additive fields, so `operator_set`/`lifecycle` may be lost on rows they rewrite. Acceptable: the fallbacks are conservative. |
| Re-upgrade after a downgrade | A row lacking `lifecycle` reads as `active`. `catalog_rev` behind head means the row was edited out-of-band, which counts as drift for rollback (§7.3). |
| schema-1 direct-dir store (`NewStore`) | New collections are opened alongside the existing six, the same way. Required for the CLI direct mode. |
| Rename series lands later | The 6 legacy collections move under plan-01..05. The `model_catalog_*` collections are untouched. Their `ai_cli` fields are already canonical. |
| Rollback of this feature's data | `aicli models rollback` per revision. Dropping the `model_catalog_*` collections returns behaviour to "all unknown". |

---

## 7. Preview / apply / rollback

### 7.1 Proposal

```jsonc
{ "id":"prop-…","state":"open|applied|expired|superseded|rejected",
  "created_at":"…","expires_at":"…(+15m)","ai_clis":["codex"],
  "base_rev":41, "policy_version":"model-policy-v1", "snapshots":["snap-…"],
  "ops":[ { "op":"add|retire|unretire|update_metadata|recommend",
            "key":"codex:gpt-6-sol",
            "pre_hash":"sha256 of canonical JSON of current row ('' if absent)",
            "before":{…}, "after":{…},
            "reasons":[{"rule":"R3","detail":"provider-declared upgrade from gpt-5.5"}],
            "evidence":"snap-…" } ],
  "blocked":[ {"key":"codex:gpt-5.4","op":"retire","reason":"partial_snapshot|stale|scope_mismatch|mass_retirement_guard|operator_set"} ],
  "observation_changes":[ {"key":"codex|sha256:…|gpt-5.5","from":"unknown","to":"unavailable"} ] }
```

- `recommend` ops change only fields that are **not** in `operator_set`, and only when the policy (§8) produced a non-empty value.
- `update_metadata` covers `display_name` only.
- A refresh run while an older proposal for the same aicli set is open marks the older one `superseded`.

### 7.2 Apply (single `dbtx` transaction)

1. **Outside every lock:** discovery has already completed and its snapshots are written.
2. Begin a `dbtx.Tx` (`internal/dbtx`, the only cross-collection boundary).
   - Read `__head__`. If it differs from `base_rev` → `proposal_stale`.
   - For each op, read the current row. If its canonical hash differs from `pre_hash` → `proposal_stale`, and nothing is written.
3. Stage writes:
   - each `backend_models` row (post-image, with `catalog_rev = head+1`)
   - revision `rev-(head+1)` `{rev, parent_rev, kind:"apply", proposal_id, actor{surface:"cli|rest|mcp", session_id?}, created_at, ai_clis, ops:[{key, pre_image, post_image}]}`
   - `__head__ = head+1`
   - proposal `state=applied`
4. Commit with intent `model-catalog-apply:<proposal_id>`. The commit is idempotent on replay, using the dbtx intent key. Never sequential writes or compensation.

A proposal larger than `dbtx.MaxOps` (1000) is split per aicli into separate proposals at preview time. If one aicli alone exceeds the limit, its proposal is `rejected` with `partial_result` and nothing is applied.

### 7.3 Rollback

- `rollback REV` creates revision `head+1` with `kind:"rollback", reverts: REV`. Its ops restore every `pre_image` of `REV`, and delete the row where `pre_image` is absent.
- **Drift check:** for every key in `REV`, the current row hash must equal `REV.post_image`'s hash. Otherwise → 409 `revision_conflict` with `conflicts[{key, expected_hash, actual_hash, touched_by_rev?}]`, and nothing is written. This catches edits by later revisions and out-of-band edits (`models tier`, old binaries).
- Rolling back the head is the common case. Rolling back an older revision is allowed whenever the drift check passes.
- Observations and snapshots are **never** rolled back. They are evidence, not intent.
- The rollback is a single `dbtx` transaction, with the same CAS on `__head__` as apply.

---

## 8. Tier, capability and quota-bucket policy (`model-policy-v1`)

The policy is deterministic and versioned. Every recommendation carries a rule id. When no rule matches, the field is left empty. **Unknown metadata is never guessed**: the model is added as `tier=unclassified, auto_assign=false` and must be classified by the operator.

| Rule | Input | Output |
|---|---|---|
| R1 seed-exact | `DefaultModels()` has the same `aicli:model` | seed `tier`, `quota_scope`, `auto_assign` |
| R2 quota-family | aicli-specific bucket mapping | codex: `codex` when `auth_mode=chatgpt`. `auth_mode=apikey` → `quota_scope="api"` plus the blocker `billing_mode_pay_per_use` (no auto-assign recommendation; backend tier unchanged). cursor: `cursorQuotaScope(id)` (existing). antigravity: `gemini` if the id has prefix `gemini-`, else `non-gemini` (matches `ObservedQuotaScope`). claude: `session`. Every other aicli: empty. |
| R3 provider-upgrade | Observed model M has `upgrade_to`=N, or another observed model declares M as its predecessor, **and** the predecessor row is retired in the same proposal | Successor inherits the predecessor's `tier` and `auto_assign`. Reason cites the provider field. Applied only when the successor's fields are not `operator_set`. |
| R4 hidden | `visibility=hidden` | `auto_assign=false` (tier from R1/R3 if any) |
| R5 no-match | none of R1/R3 | `tier=unclassified`, `auto_assign=false` |

**Shared buckets.** Every model of one aicli that maps to the same `quota_scope` shares one `backend_quotas` row (`aicli:scope[:window]`), as today. The policy never creates quota rows. If a recommended scope has no quota row, the proposal reports `quota_bucket_missing` and routing treats it as unknown headroom (eligible, as today).

**Routing eligibility (resolver change).** `EvaluateCandidates` adds a check after "model disabled":

- `lifecycle ∈ {retired, excluded}` → reject `model retired`. An explicit pin of that exact pair is still allowed, with a warning.
- Active-scope `launch_rejected_until > now` → reject `model rejected at launch until …`, unless explicitly pinned.
- `availability=unavailable` (active scope) → reject `model unavailable for active account`, unless explicitly pinned.
- `unknown` / `stale` / `unsupported` → eligible, as today.
- Ranking: within the headroom tie pool, `available` sorts before `unknown`/`stale`/`unsupported`, ahead of the round-robin.
- `CandidateEvaluation` gains `availability` and `lifecycle` (additive JSON).

---

## 9. Automatic startup failure recovery (DEFERRED)

**Status: deferred by operator decision; not implemented and not delivered.** There is no launch-rejection detector, no automatic relaunch, no prompt-withheld launch path and no `model_recovery.*` configuration. A launch that fails with an invalid, retired or not-entitled model is reported like any other launch failure and is never silently substituted.

**Why deferred.** Safe automatic recovery needs a trustworthy, versioned, adapter-level signal that a rejection happened before the CLI read or acted on the task prompt, plus a deterministic way to tell it from a post-prompt failure. That depends on the PTY runtime migration and the redesign of state, prompt and error detection. Reading pane text from tmux today cannot prove the ordering, and a wrong guess would relaunch a started task or poison routing evidence.

**Criteria for revisiting** (tracked in `docs/FUTURE_ENHANCEMENTS.md`): detection comes from the PTY runtime's structured state/error events, not pane scraping; per-adapter, per-version signatures proven pre-prompt; at most 3 distinct same-aicli, same-tier attempts under one deadline; pins, restores and forks never substituted; no permission, billing or cross-aicli widening; every attempt audited and persisted.

**Manual alternative (supported today).** The operator refreshes and applies the catalog, then reselects the model of an affected agent with `warden switch`; see `docs/RUNBOOK-aicli-model-catalog.md`. The resolver still honours a recorded `launch_rejected_until` observation; nothing in this release writes one automatically.

---

## 10. Startup readiness and pickers

- Daemon start **does not** run discovery. Seeding (D4-fixed) and routing read only stored observations.
- The TUI spawn picker (`loadSpawnCandidatesCmd`) and `GET /api/v1/models` get an availability badge (`available`, `unknown`, `stale`, `unavailable`, `retired`, `rejected`). `retired` and `excluded` rows are hidden by default (TUI toggle `a`: show all). The web has no picker today; it gains the read-only catalog view in operator-api-cli-and-surfaces only if that task's scope allows. Otherwise it is deferred and documented.

## 11. Locking and I/O rules

- Provider I/O (exec, file reads of CLI caches, app-server RPC) **never** runs under `backendstore.Store.mu`, a dbtx transaction, or any daemon-global lock.
- Order: resolve scope → discover → **then** open the store transaction. Snapshot writes are a short transaction after I/O.
- Concurrent refreshes for overlapping aicli sets: the second proposal supersedes the first. A concurrent apply loses CAS with `proposal_stale`, never a partial write.

## 12. Coordination

- **backend→aicli rename series (plan-c6748891 … plan-26159587):**
  - The six legacy collections keep their names and remain the sole read/write authority. No renames, no mirrors, no dual writes.
  - New collections are named `model_catalog_*`, outside the rename mapping, and use the canonical `ai_cli` field.
  - The CLI adds only the `aicli models …` subtree with retained `backend model …` / `models …` aliases. It does not move the other `backend` commands.
  - Workers re-check plan-01's status before merging. If its `foundations-and-aliases` has landed, they reuse its alias-conflict helper instead of adding one.
- **PTY02/PTY03:** automatic launch recovery (§9) is deferred until the PTY migration and state/prompt/error detection redesign land; nothing else here depends on them.
- **Daemon API is spec-first:** `openapi.yaml`, then `make generate`. No handwritten DTOs. Run `make gendocs` after any cobra change (CI `gendocs-check`).
- **Integrity contract** (`2026-10-09-backend-registry-integrity-contract.md`): the new collections join `Verify`/`Repair` coverage in catalog-snapshots-and-policy.

## 13. Downstream tasks and test matrix

| # | Task | Implements | Required tests (at least) |
|---|---|---|---|
| 1 | **catalog-snapshots-and-policy** | §5 schemas, collections and retention; `TierUnclassified`; additive `ModelEntry` fields; D4 seeder rules (§6.3); D6 `ProfileSource` wiring; integrity coverage | Seeder never resurrects excluded/retired rows; never prunes discovered/operator_set rows. Old-binary fixture round-trip of additive fields. Retention bounds. Key-length fallback. |
| 2 | **provider-discovery-adapters** | §3 interface for codex, cursor, antigravity, opencode, crush; `unsupported` for claude, aider, goose; D1, D2, D8; `ModelLister` reimplemented as a projection | Fixture parsing per aicli and version. Codex `CODEX_HOME`, identity mismatch, stale cache, bundled excluded, config_hint separate. agy tab and legacy formats. Crush/opencode marked P. Timeout and 4 MiB cap. A test asserting no generation subcommand is ever exec'd (argv allowlist). |
| 3 | **tier-and-quota-policy** | §8 R1–R5; resolver eligibility and ranking; seed refresh (D3) | The gpt-5.5 case end-to-end on fixtures: retire gpt-5.5, successor via R3 or unclassified. apikey blocker. Hidden → no auto-assign. Unknown → unclassified. Resolver rejects retired/unavailable/rejected but allows explicit pins. |
| 4 | **preview-reconcile-apply-and-rollback** | §6.1/§6.2 transitions, §7 proposals and revisions, dbtx apply and rollback | Full transition table, table-driven. Every error class → no retirement. Mass-retire guard. CAS staleness on head and pre_hash. Expiry and supersede. Rollback drift conflict. Crash mid-commit (dbtx crashtest harness): all-or-nothing. MaxOps split. |
| 5 | **bounded-startup-failure-recovery** (DEFERRED, §9; not delivered, no detector, no retry) | §9 detector, budget, per-surface flow; D5; D7 | Exactly ≤3 distinct attempts; shared deadline honoured; same pair never retried; pinned/restore/fork never substituted. Post-prompt failures not handled here. Rejection TTL. D5 regression test. |
| 6 | **operator-api-cli-and-surfaces** | §4 CLI/REST/MCP, aliases, discover compat, TUI badges, `make generate` + `make gendocs`, D9 | Alias conflict exit 2. `--json` == REST body. Exit codes. Direct vs daemon mode parity. OpenAPI codegen clean. MCP parity. |
| 7 | **observability-and-adversarial-verification** | Events, metrics (`model_catalog_refresh_total{aicli,completeness,error}`, `model_launch_attempts_total{outcome}`), audit entries for apply/rollback, adversarial suite | Malicious or huge CLI output (ANSI, NUL, 10 MB, shell metachars in ids stay shell-quoted on the launch line). Account switch between refresh and spawn. Concurrent refresh/apply. Partial output disguised as complete. Secrets never in snapshots or logs (grep test on fixtures with planted tokens). |
| 8 | **rollout-documentation-and-release-gates** | README, docs/FEATURES.md, USAGE.md, site guide + generated `reference/cli.md`, `skills/warden`, release notes (run refresh after upgrade), upgrade/downgrade matrix (§6.4) verification, tag (minor) after maintainer confirmation | Upgrade from the previous release's store fixture: routing unchanged without refresh. Downgrade fixture: no auto-routing of retired rows. Gendocs check. Docs checklist per CLAUDE.md. |

Ordering: 1 → (2, 3 in parallel) → 4 → (5, 6 in parallel) → 7 → 8. Task 5 depends only on 1 (observation writes) and on the resolver hook from 3.
