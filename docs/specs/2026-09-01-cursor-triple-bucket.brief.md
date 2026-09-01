# Cursor triple-bucket catalog + usage — implementation brief

**Date:** 2026-09-01
**Job:** `analyze` → handoff to `implement` in pipeline `cursor-triple-bucket`
**Status:** Live-verified. Docs-only. No production code in this commit.
**Authoritative catalog:** `cursor-agent --list-models` (CLI 2026.08.31-4057e58) ≡ `warden models --backend cursor` (214 ids). Prefer live ids over any abbreviated family names in the operator prompt when they disagree.

Companion table (paste source for `DefaultModels`): [`2026-09-01-cursor-triple-bucket.seed.tsv`](./2026-09-01-cursor-triple-bucket.seed.tsv).

---

## (a) Confirmed live model id list

`cursor-agent --list-models` and `warden models --backend cursor` emit the **same 214 ids**. There is **no** `cursor-agent usage` subcommand (`--help` lists `status|whoami`, `about`, `models`, not usage). Interactive TUI has a feature-flagged `/usage` pager (`enableUsageCommand`) that is **not** a CLI verb.

Live ids grouped by usage bucket (selectors in §c):

### Auto (1)

`auto`

### Included / Cursor Models (16) — `composer-*` and `cursor-grok-*`

`composer-2.5`, `composer-2.5-fast`,
`cursor-grok-4.5-low`, `cursor-grok-4.5-low-fast`, `cursor-grok-4.5-medium`, `cursor-grok-4.5-medium-fast`, `cursor-grok-4.5-high`, `cursor-grok-4.5-high-fast`,
`cursor-grok-4.6-low`, `cursor-grok-4.6-low-fast`, `cursor-grok-4.6-medium`, `cursor-grok-4.6-medium-fast`, `cursor-grok-4.6-high`, `cursor-grok-4.6-high-fast`, `cursor-grok-4.6-xhigh`, `cursor-grok-4.6-xhigh-fast`

### API / Other Models (197)

Everything else on the live menu: Codex 5.3 (8), GPT-5.2 (+ variants), GPT-5.5, GPT-5.6 sol/terra/luna, GPT-5.4 / mini / nano, GPT-5.1, GPT-5 Mini, Claude Opus 5 / 4.8 / 4.7 / 4.6 / 4.5, Claude Fable 5 / 5.1, Claude Sonnet 5 / 4.6 / 4.5 / 4, Gemini 3.7/3.6/3.5/3 flash + 3.1 Pro, Kimi, GLM.

**Prompt vs live (live wins):**

| Prompt abbreviation | Live fact |
|---|---|
| “GPT-5.4 / 5.2 / 5.1 full matrix” | No `gpt-5.4-low-fast`. No bare `gpt-5.4`. `gpt-5.2` exists as a distinct id plus effort variants. |
| “Fable 5 thinking variants” | Live also has **Fable 5.1** (`claude-fable-5-1-*`) and non-thinking Fable 5. |
| “Gemini 3.6-flash-*” | Live uses `gemini-3.6-flash-minimal` (not a `none` effort). |
| `autoBucketModels` from the usage API | **Stale** vs the live menu: includes `vega*`, `composer-1*`, unprefixed `grok-4.5*`, **omits all `cursor-grok-4.6-*`**. Do **not** use it as the included selector. |

Do not register ids that are not in the live list (no `claude-3-opus`, no `sonnet-3.7`, no `vega`).

---

## (b) Exact seed entries (Tier / AutoAssign / DisplayName)

Keep every **non-cursor** `DefaultModels()` row unchanged. Replace the three stale cursor rows:

| Remove | Today |
|---|---|
| `cursor` / `claude-3-opus` | tier-1 AutoAssign |
| `cursor` / `sonnet-3.7` | tier-2 AutoAssign |
| `cursor` / `composer-2.5-fast` | tier-3 AutoAssign — **re-insert** with the same tier/AA, live DisplayName `Composer 2.5 Fast` |

Insert **214** cursor rows from the TSV. All `Enabled: true`. `AutoAssign: true` **only** on the seven operator-locked faces:

| Tier | Bucket | ModelID | DisplayName (live) | AutoAssign |
|---|---|---|---|---|
| tier-1 | included | `cursor-grok-4.6-high-fast` | Cursor Grok 4.6 Fast | **true** |
| tier-1 | api | `claude-opus-5-thinking-high` | Claude Opus 5 1M Thinking | **true** |
| tier-1 | auto | — | — | none |
| tier-2 | included | `cursor-grok-4.5-high` | Cursor Grok 4.5 | **true** |
| tier-2 | auto | `auto` | Auto (default) | **true** |
| tier-2 | api | `claude-sonnet-5-thinking-high` | Claude Sonnet 5 1M Thinking | **true** |
| tier-3 | included | `composer-2.5-fast` | Composer 2.5 Fast | **true** |
| tier-3 | auto | — | — | none |
| tier-3 | api | `gemini-3.7-flash-high` | Gemini 3.7 Flash | **true** |

`internal/router/resolver.go` skips `!AutoAssign` unless the caller named that exact model. `internal/daemon/backend_recovery.go` `policyCandidates` also requires `AutoAssign`. One face per bucket per tier is therefore the entire automatic routing/recovery surface; the other 207 ids exist for explicit `--model` and `warden models` / `GET /api/v1/models`.

### Tier heuristic (confirmed + refined)

Applied to every live id in the TSV:

| Tier | Rule |
|---|---|
| **tier-1** | Opus 5 / 4.8 / 4.7 **high+** (high, xhigh, max, and their thinking/fast forms); all `claude-4.6-opus-*`; Fable **thinking-high+** only (`claude-fable-5-thinking-{high,xhigh,max}` and Fable 5.1 thinking-high+); Grok **4.6 high/xhigh** (not medium/low); GPT-5.5 **extra-high**; GPT-5.6-sol **xhigh/max**; Codex **xhigh**. |
| **tier-2** | `auto`; Composer 2.5 (non-fast); Grok 4.5 except low; Grok 4.6 **medium**; all Sonnet 5 / 4.x sonnet; mid Opus (low/medium) and Opus 4.5; Fable non-thinking + thinking-low/medium; GPT-5.6 terra/luna (all efforts); GPT-5.6-sol below xhigh; GPT-5.5 below extra-high; GPT-5.4 (not mini/nano); GPT-5.2; Codex default/high (not low, not xhigh); Gemini 3.1 Pro. |
| **tier-3** | Composer-2.5-fast; Grok **low**; all Gemini **flash**; GPT-5.4 mini/nano; GPT-5.1; GPT-5 Mini; Codex **low**; Kimi; GLM. |

Refinements vs the operator sketch (locked AutoAssign faces still win):

- Opus 4.5 high is **tier-2** (heuristic t1 list is 5 / 4.8 / 4.7 / 4.6, not 4.5).
- Fable **non-thinking** high/xhigh/max is **tier-2** (only *thinking-high+* is t1).
- GPT-5.4 xhigh stays **tier-2** (not in the t1 GPT list).
- `gpt-5.6-sol-none/low/medium/high` stay **tier-2**; only sol xhigh/max are t1.

Resulting cursor counts: **54 tier-1 / 124 tier-2 / 36 tier-3**. Whole-catalog `DefaultModels()` after the change: **236** entries (22 unchanged non-cursor + 214 cursor). Previous totals in `models_test.go` (`Len 25`, t1=5, t2=9, t3=11) must move to **236 / 58 / 132 / 46**.

DisplayName: copy the live menu text after ` - ` verbatim (keep `(NO ZDR)` suffixes).

Implementation hint: a small helper `cursorModel(id, display string, tier ModelTier, autoAssign bool) ModelEntry` keeps `seed.go` readable. Do not generate the catalog at runtime from `cursor-agent --list-models` — the seed is the committed source of truth.

### Seed prune / migration caveat (required)

`seedDefaultsIfEmpty` (`internal/backendstore/store.go`):

1. **Deletes** non-custom models whose `(backend, id)` is absent from `DefaultModels()`. Existing installs **drop** `claude-3-opus` and `sonnet-3.7` automatically on next store open.
2. **Inserts** missing default keys (the 214 live ids, including the seven faces).
3. **Does not rewrite `Tier` or `DisplayName`** on already-present keys. It only backfills `AutoAssign` when the stored record **lacks** the `auto_assign` field.

Consequences:

- `composer-2.5-fast` already exists as tier-3 AutoAssign — it is **not** retiered. That happens to match. If an operator previously mutated its tier, that mutation **sticks**.
- New faces (`cursor-grok-4.6-high-fast`, `auto`, …) are fresh inserts with the correct flags.
- Stale AutoAssign on pruned ids goes away with the prune.

**Do not** rewrite every existing cursor row's tier. Optional one-shot (only if you want faces to be correct even after prior mutations): after seeding, for the seven locked `(cursor, model_id)` keys that are **not** `IsCustom`, set `Tier`/`AutoAssign`/`DisplayName` from the new seed. Document it. Do not add a “wipe and reseed” CLI in this job.

Custom models (`IsCustom`) are never pruned.

---

## (c) Three Limit windows

Emit **exactly three** `backendusage.Limit` rows. Never flatten to one percent. Mirror Codex multi-window + the Antigravity dual-window fixture (`internal/backendusage/testdata/scoped-limits.json`, `recovery.Rank` antigravity test).

| ID | Scope | Label | ModelFamilies | Models | UsedPercent | ResetsAt |
|---|---|---|---|---|---|---|
| `cursor:included` | `included` | `Included` | `["composer","cursor-grok"]` | `null` | `planUsage.totalPercentUsed` | `billingCycleEnd` |
| `cursor:auto` | `auto` | `Auto` | `null` | `["auto"]` | `planUsage.autoPercentUsed` | `billingCycleEnd` |
| `cursor:api` | `api` | `API` | `["claude","gpt","gemini","kimi","glm"]` | `null` | `planUsage.apiPercentUsed` | `billingCycleEnd` |

Labels match the Cursor Pro CLI overlay / web portal (“Included”, “Auto”, “API”). `normalizeLimits` sorts families; IDs sort as `cursor:api`, `cursor:auto`, `cursor:included`.

### Why these selectors (`recovery.applies`)

`internal/recovery/rank.go` `applies`:

- Empty `Models` **and** empty `ModelFamilies`: if `scope` has prefix `non-`, exclude models containing the rest; else **match everything**.
- Else exact `Models` or `strings.Contains(lower(model), lower(family))`.

Therefore:

- **Never** leave the API window with empty selectors and `scope: "api"` — that would apply to composer/grok/auto too.
- **Never** use `scope: "non-composer"`; the `non-` trick only excludes one substring.
- Included families `composer` + `cursor-grok` match the 16 included ids and nothing in the API set.
- Auto uses **exact** `Models: ["auto"]` (safer than family `"auto"`).
- API families cover every live non-included id (`gpt-5.3-codex-*` contains `gpt`).
- Do **not** put `autoBucketModels` into `Models` for included — it is missing Grok 4.6 and stuffed with retired aliases.

If a percent is omitted in JSON, leave `UsedPercent` **null**. Do **not** copy the CLI’s `?? 0` default. Unknown remains trial-eligible (`Rank` sorts known headroom first; coordinator skips `headroom == 0`).

`RemainingPercent`: if used is in `[0,100]`, set `100-used` (Codex pattern). `DurationMinutes`: omit (monthly cycle is not a minute window). `LimitState`: `"reached"` on a row whose used is `>= 100`; else omit.

`Status`: `StatusOK` when authenticated and three rows emitted; `StatusRateLimited` if **any** row is reached (Codex). Ranking still uses per-model headroom, so an exhausted API window does **not** exclude Grok/Composer/`auto`.

Live 2026-09-01 probe (Pro plan, do not hard-code these percents): included `totalPercentUsed≈8.87`, auto `≈4.09`, api `=100`, `displayMessage="You've hit your usage limit"`. That is the motivating case: recovery must leave Opus and pick an included/auto face.

`ResetsAt`: parse `billingCycleEnd`. Connect-JSON encodes proto int64 as a **string of milliseconds** (`"1790879487000"` → `2026-10-01T18:31:27Z`). Accept string or number. Same reset on all three rows (one monthly cycle). Live start was `2026-09-01T18:31:27Z` (30 days).

Do **not** use `includedSpend/limit` as the included percent. Live: `includedSpend=2000`, `limit=2000` (cents, the $20 Pro purse) while `totalPercentUsed≈8.87`. The overlay bar is `totalPercentUsed`. Spend cents stay out of `Limit` (no dollar leakage).

---

## (d) Chosen usage data source + parse plan

### Investigation order (done)

1. **CLI flags / subcommands** — no `usage`. `status --format json` and `about --format json` only yield auth + `subscriptionTier`. Keep them for auth/plan fallback.
2. **Dashboard API (chosen)** — Connect-RPC JSON on `https://api2.cursor.sh` with the existing Cursor login token.
3. **Local state** — `~/.config/cursor/auth.json` is `{accessToken, refreshToken}` only. `~/.cursor/cli-config.json` has no cached percents. `statsig-cache.json` is unrelated. No local usage cache to parse.
4. Fallback only if HTTP fails after auth: still emit the three Limit rows with **null** percents (not `StatusUnsupported`).

### Proven live call

```
POST https://api2.cursor.sh/aiserver.v1.DashboardService/GetCurrentPeriodUsage
Authorization: Bearer <accessToken from auth.json>
Content-Type: application/json
Connect-Protocol-Version: 1

{}
```

HTTP 200, `application/json`. This is the **same RPC** the feature-flagged CLI usage pager already calls (`getCurrentPeriodUsage` + `getHardLimit` + `getPlanInfo` in cursor-agent `2026.08.31-4057e58`).

Token path (from cursor-agent’s own loader):

| OS | File |
|---|---|
| Linux | `$XDG_CONFIG_HOME/cursor/auth.json` or `~/.config/cursor/auth.json` |
| macOS | `~/.cursor/auth.json` |
| Windows | `%AppData%/Cursor/auth.json` |

Bearer against this file **worked** on this host. `https://www.cursor.com/api/usage` returned 401 (cookie session, ignore). `https://api2.cursor.sh/auth/usage` is the legacy gpt-4 request counter (ignore).

Optional companion: `GetPlanInfo` → `planInfo.planName` (`"Pro"`). Can replace `about` for `Account.Plan`. `GetHardLimit` is on-demand spend (`noUsageBasedAllowed`); not a third usage bar.

### Parse map (`GetCurrentPeriodUsageResponse`, camelCase JSON)

```
billingCycleStart, billingCycleEnd     int64-ms (string or number)
planUsage.totalPercentUsed             → cursor:included UsedPercent
planUsage.autoPercentUsed              → cursor:auto UsedPercent
planUsage.apiPercentUsed               → cursor:api UsedPercent
planUsage.{auto,api,total}_percent_used snake_case: accept both
autoBucketModels                       ignore for selectors (stale)
displayMessage                         do not copy into Error.Message (may leak plan copy); use for LimitState only if percent >= 100
```

Proto field list (for the Go struct): `PlanUsage` has `total_spend`, `included_spend`, `bonus_spend`, `remaining`, `limit`, `auto_spend`, `api_spend`, `auto_limit`, `api_limit`, `auto_percent_used`, `api_percent_used`, `total_percent_used`. Spend fields are **cents**; do not surface them on `Limit`.

### Adapter shape

Move `CursorAdapter` off the “unsupported after about” path in `internal/backendusage/simple.go` into **`internal/backendusage/cursor.go`** (mirror `codex.go`).

`Fetch` sequence (stay inside `ProviderTimeout` = 3s — today’s serial `status`+`about` is already tight):

1. If `!b.Installed` → `notInstalled`.
2. `cursor-agent status --format json` (existing). Unauthenticated / malformed / command failure: same as today.
3. Read auth.json (injectable `ReadFile` / `AuthPath`). Missing token after a successful status: still emit three null-% Limits + `Account` from about/GetPlanInfo if cheap; do not return `unsupported`.
4. HTTP `GetCurrentPeriodUsage` with injectable `Doer` (httptest in unit tests). Default endpoint `https://api2.cursor.sh`; honor `CURSOR_API_ENDPOINT` if set.
5. On HTTP/parse failure: **StatusOK**, three Limits, null percents, `Account` from status/about. Recovery can still `applies()` by family. Do not cache a lie of `unsupported`.
6. On 401: do not invent percents. Prefer `StatusError`/`unavailable` (transient → stale cache) rather than wiping the three-row contract. Token refresh is a **follow-up** (refreshToken is in auth.json; do not block this job on a refresh client).
7. Skip `about` when `GetPlanInfo` is fetched in parallel, or keep `about` only as plan fallback. **Budget:** status + HTTP must finish under 3s; parallelize.

`CURSOR_API_KEY` users may not have auth.json. If status is authenticated only via API key and GetCurrentPeriodUsage 401s, emit three null-% rows (same fallback). Do not scrape the TUI.

Sanitize: no email, no token, no raw provider payload in `Result.Error` or `Account.Label` (service already redacts `Label` on cache).

---

## (e) Files to touch

| File | Change |
|---|---|
| `internal/backendstore/seed.go` | Replace 3 stale cursor rows with 214 live entries (helper OK). |
| `internal/backendstore/models_test.go` | Totals 236 / 58 / 132 / 46; `GetModel` the seven faces + prune assertions for `claude-3-opus` / `sonnet-3.7`. Persist test that already-present `composer-2.5-fast` is not retiered if mutated. |
| `internal/backendusage/simple.go` | Delete `CursorAdapter` (leave Claude/Antigravity/Generic). |
| `internal/backendusage/cursor.go` | New adapter + parse + auth-path + HTTP. |
| `internal/backendusage/cursor_test.go` | See §f. |
| `internal/backendusage/testdata/cursor-period-usage.json` | Sanitized live-shaped fixture (camelCase, string ms timestamps, three percents, `autoBucketModels` present but unused). |
| `internal/backendusage/service.go` | No logic change unless adapter registration moves; `NewService` still includes `CursorAdapter{}`. |
| `internal/recovery/rank_test.go` | Cursor three-bucket applies/headroom test (below). |
| `docs/FEATURES.md` §35 usage paragraph | Cursor now supplies three windows. |
| `site/src/content/docs/reference/backend-registry.md` | Same; keep Antigravity as still-unsupported. |
| `docs/USAGE.md` | Only if it still says Cursor usage is unsupported (the spend/backends section). |
| `docs/agent-backends/cursor.md` | One paragraph: catalog is seeded from live menu; usage is dashboard RPC, not a CLI verb. |
| `skills/warden/references/operations.md` | If it repeats the unsupported sentence. |
| `internal/agentbackend/backends/testdata/cursor/models.txt` | **Optional** refresh so `TestParseCursorModels` stays representative. Parser tests currently pin old ids (`composer-2.5-fast`, `claude-opus-4-8-thinking-high`) that still exist. Refresh only if tests would otherwise rot. |

**Do not edit `internal/cli/**`.** `warden usage` already prints whatever the daemon snapshot contains; `usage_test.go` uses synthetic multi-limit rows. Peer agents own CLI help redesign.

Leave `internal/router/resolver_test.go` and `quota_test.go` strings `claude-3-opus` / `sonnet-3.7` alone unless a test **loads the seed and GetModel** those ids — `RecordQuotaUsage` takes a free-form model string.

`internal/lifecycle/tier_trio_integration_test.go` does not register a cursor backend row, so extra cursor seed models stay ineligible. Totals in that file are not catalog-length assertions.

OpenAPI / `gendocs`: no CLI help change ⇒ no `make gendocs`. Usage JSON schema already has multi-limit fields.

---

## (f) Tests to add/update

1. **Seed catalog**
   - Fresh store: 236 models; cursor count 214; exactly seven cursor `AutoAssign`.
   - `GetModel("cursor", "claude-3-opus")` / `sonnet-3.7` → `ErrModelNotFound`.
   - Each AutoAssign face has the locked tier.
   - Persist + reopen: pruned ids stay gone; a mutated non-custom `composer-2.5-fast` tier is **not** overwritten (current seed semantics); new ids appear.

2. **CursorAdapter** (httptest + fake `CommandRunner` + fake auth file)
   - Happy path: fixture → three Limits, correct id/scope/label/families/models, percents, `ResetsAt` from ms string, `Account.Plan` from about or GetPlanInfo.
   - API 100% → that row `LimitState=reached`, `StatusRateLimited`, other rows still present with their percents.
   - Missing `autoPercentUsed` → `UsedPercent` null on auto (never 0).
   - HTTP 500 after authenticated status → StatusOK + three null-% rows (contract preserved).
   - Unauthenticated status → `StatusUnauthenticated`, empty usage.
   - Not installed → `notInstalled`.
   - Duplicate-ID / empty scope rejected by existing `normalizeLimits` (don’t emit blank ids).
   - Int64 billing end as JSON number **and** string.

3. **recovery.Rank**
   ```
   limits: included 10% (families composer, cursor-grok),
           auto 4% (models ["auto"]),
           api 100% (families claude,gpt,…)
   candidates: cursor/claude-opus-5-thinking-high,
               cursor/cursor-grok-4.6-high-fast,
               cursor/auto,
               cursor/composer-2.5-fast
   ```
   Assert opus matches **only** api (headroom 0); grok and composer match **only** included (headroom 90); auto matches **only** auto (headroom 96). No candidate inherits all three percents.

4. **Do not** add a live `cursor-agent` integration test in CI (network + secrets). Fixture is enough.

---

## (g) Docs DoD

Walk the feature-delivery checklist:

| Item | This job |
|---|---|
| Tag & release | **Skip** (implement + review land first; confirm with maintainer before any `v*` tag). |
| `README.md` | Skip unless it still names `claude-3-opus` / Cursor usage-unsupported (it does not today). |
| `docs/FEATURES.md` | **Update** §35: Codex structured; **Cursor three buckets**; Claude/Antigravity still unsupported. |
| `docs/USAGE.md` | Update the usage/backends sentence if it still groups Cursor with unsupported. |
| `docs/specs/` | This brief + TSV **are** the spec. Recovery spec already allows multi-window providers. |
| Website `site/src/content/docs/reference/backend-registry.md` | **Update** the “Claude and Cursor … unsupported” paragraph; show a cursor three-row JSON example next to the antigravity fixture (antigravity remains hypothetical/`unsupported`). |
| `site/.../guides/backend-superpowers.md` | Optional: one line that `wd models` ids should be seeded. |
| Skill `skills/warden/` | Update operations/backend-registry copy if it repeats unsupported. |
| CLI help / `make gendocs` | **Skip** (no cobra `Use`/`Short`/`Long`/flag changes). |
| `docs/agent-backends/cursor.md` | Short usage + catalog note. |

Do **not** hand-edit generated `site/src/content/docs/reference/cli.md`.

---

## (h) Residual risks / follow-ups

1. **`autoBucketModels` drift** — API list omitted Grok 4.6 on 2026-09-01. Family selectors are the defense. Revisit if Cursor splits included further.
2. **Auth token refresh** — access JWT will expire. `status` may refresh it as a side effect; if 401s appear in the wild, add a refresh-token POST (follow-up, not this job).
3. **ProviderTimeout 3s** — serial status+about+HTTP will miss the budget. Parallelize / drop about.
4. **API-key-only CLI** — `CURSOR_API_KEY` may never see `GetCurrentPeriodUsage`. Null-% three-row fallback is the contract; document it.
5. **Seed does not retier existing keys** — operator-mutated `composer-2.5-fast` can disagree with the locked face. Optional seven-key backfill only.
6. **Catalog churn** — Cursor’s menu changes often (`testdata/cursor/models.txt` is already stale vs 214 live ids). Seed is a snapshot; a later job can refresh. Do not auto-scrape at daemon boot (non-deterministic routing).
7. **`enableUsageCommand`** — if Cursor later ships `cursor-agent usage --format json`, prefer it over HTTP **only** if it is stable, documented, and still three windows. Until then HTTP is the source.
8. **Do not scrape** interactive `/usage` or invent percents from `includedSpend/limit`.
9. **Coordination** — other agents are on CLI help redesign and autopilot hierarchy. Stay out of `internal/cli/**`. `seed.go` / `backendusage` / `recovery` / docs are the lane.
10. **Privacy** — never log Bearer tokens or emails from `status.userInfo`. Tests use synthetic tokens.

---

## Implementer checklist (copy)

- [ ] Replace cursor rows in `DefaultModels()` from the TSV (214 ids, 7 AutoAssign faces, live DisplayNames).
- [ ] Update `models_test.go` counts and prune/face assertions.
- [ ] Implement `CursorAdapter` HTTP parse of `GetCurrentPeriodUsage`; three Limits; no flatten.
- [ ] Fixture + unit tests; Rank applies test for the three buckets.
- [ ] Docs: FEATURES §35, backend-registry reference, cursor gap doc.
- [ ] No `internal/cli/**`, no `make gendocs`, no tag.

## Definition of done (for implement)

A Cursor Pro install with `~/.config/cursor/auth.json` yields `warden usage --json` `status: "ok"` (or `rate_limited` when a bar is at 100) with **three** cursor limits `included` / `auto` / `api`. Recovery ranking of `claude-opus-5-thinking-high` vs `cursor-grok-4.6-high-fast` vs `auto` uses **different** headrooms. Fresh backend store contains 214 cursor models and not `claude-3-opus`.
