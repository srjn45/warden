# Per-scope quota routing — spec freeze

- **Status:** Locked (Phase 0 — spec freeze; docs only, no production code)
- **Repo:** warden. Baseline: `autopilot/per-scope-quota-routing` integration branch.
- **Scope:** Freeze the design that makes quota headroom **model-scope-aware** so
  the resolver, recovery, and TUI spawn flow stop treating every model on a
  backend as sharing one headroom value. This document is normative: the
  decisions in §2 are **locked** and are not to be relitigated during
  implementation.

---

## 0. Why this exists

Today `backendstore.BackendQuota` is keyed by `backendID` alone. The resolver's
`EvaluateCandidates` calls `GetHeadroom(backendID, now)`, which returns **one**
headroom / limited flag per backend. Every model on that backend therefore sees
the same quota state.

That is wrong for multi-scope providers:

- **Cursor** has three independent quota scopes — `api`, `auto`, `included` —
  with distinct model sets and independent limits. When `cursor/api` hits 100%,
  every cursor model is blocked even though `cursor/auto` still has ~55%
  remaining and `cursor/included` may be nearly empty.
- **Antigravity** has two scopes — `gemini` and `non-gemini` — and may expose
  multiple windows per scope (e.g. 5-hour + weekly). Exhausting the non-gemini
  window must not zero out gemini headroom.

Live usage already carries the right scope string on each
`backendusage.Limit` (`Scope` field; see
[`2026-09-01-cursor-triple-bucket.brief.md`](2026-09-01-cursor-triple-bucket.brief.md)).
The store and resolver do not yet consume it. This spec freezes the contract
that plugs that gap end-to-end: scoped quota records, per-model scope tags,
model-aware headroom lookup, quota sync from live snapshots, scoped
`LimitedUntil`, canonical seed scopes, and a tier-first TUI spawn flow.

### Relationship to prior specs

- Builds on tiered routing
  ([`tiered-model-routing.plan.md`](tiered-model-routing.plan.md)) and the
  Cursor triple-bucket catalog
  ([`2026-09-01-cursor-triple-bucket.brief.md`](2026-09-01-cursor-triple-bucket.brief.md)).
- Complements reactive hard-limit recovery
  ([`2026-09-01-reactive-backend-limit-recovery.md`](2026-09-01-reactive-backend-limit-recovery.md)):
  recovery remains reactive; this change makes the **eligibility signal**
  per-model-scope instead of backend-wide.
- Does **not** rewrite the `backendusage` provider adapters or change how
  live `Limit` rows are scraped — only how they are written into
  `backendstore` and consumed by the resolver / TUI.

---

## 1. Problem (normative statement)

| Today | Required |
|---|---|
| One `BackendQuota` record per backend; store key = `backendID` | One record per `(backendID, scope)`; store key = `backendID + ":" + scope` |
| `GetHeadroom(backendID, now)` is the resolver's only headroom API | New `GetModelHeadroom(backendID, modelID, now)`; `GetHeadroom` kept as min-across-scopes |
| `ModelEntry` has no quota-scope tag | `ModelEntry.QuotaScope`; blank → `"default"` |
| Live Snapshot usage is not projected into scoped `BackendQuota` rows | After every successful Snapshot (and on daemon startup), write one `BackendQuota` per returned limit, keyed by `Limit.Scope` |
| `Backend.LimitedUntil` stamps the whole backend | Backend row limited only when **all** scopes are limited; per-scope `BackendQuota.LimitedUntil` is authoritative for the resolver |
| TUI spawn picks a backend CLI from a dropdown | Tier selector + live candidate table; spawn passes `ResolveOptions.Tier` |

Motivating case (Cursor Pro, live-verified in the triple-bucket brief): API at
100%, Auto ~4–55% remaining, Included nearly empty — recovery and routing must
leave exhausted API faces and still pick an included/auto AutoAssign face.

---

## 2. Locked design decisions

| # | Decision | Choice |
|---|---|---|
| **D1** | **Scoped `BackendQuota`** | `BackendQuota` gains `Scope string`. Store key becomes `backendID + ":" + scope` (e.g. `"cursor:auto"`, `"antigravity:gemini"`). Existing scope-less records migrate on first read: `Scope` set to `"default"`, key rewritten. `GetHeadroom(backendID, now)` **stays** and returns `min(headroom)` across all scopes for that backend. |
| **D2** | **Model → scope tag** | `ModelEntry` gains `QuotaScope string`. Each seed model is tagged with its quota scope. Blank `QuotaScope` means the model uses `"default"` scope. |
| **D3** | **`GetModelHeadroom`** | New `GetModelHeadroom(backendID, modelID string, now time.Time)` on the Store interface. Looks up the model's `QuotaScope`, loads that `BackendQuota` record, returns its headroom. For backends with multiple windows per scope (e.g. antigravity 5-hour + weekly), returns `min(headroom)` across all windows for that scope. |
| **D4** | **Resolver uses model headroom** | `EvaluateCandidates` replaces `GetHeadroom` with `GetModelHeadroom`. The `limited` flag on a candidate reflects the **model's scope** limit, not a backend-wide flag. |
| **D5** | **QuotaSync from live Snapshot** | After `backendusage.Service.Snapshot` fetches live usage, write per-scope `BackendQuota` records into the backendstore (one record per `Limit` returned). The `Limit.Scope` field already carries the right string. Sync runs after every successful Snapshot and on daemon startup. |
| **D6** | **Scoped vs backend `LimitedUntil`** | `Backend.LimitedUntil` is set only when **ALL** scopes for a backend are limited (backward compat for callers that still read the backend row). `BackendQuota.LimitedUntil` tracks per-scope hard-limit recovery. The resolver checks **scoped** `LimitedUntil` first. |
| **D7** | **Canonical seed scopes** | Scope definitions are seed-level and canonical — see §3. |
| **D8** | **TUI spawn = tier + candidates** | Replace the per-backend CLI dropdown with a tier selector (`auto` / `tier-1` / `tier-2` / `tier-3`). Below it, a live candidate table shows each AutoAssign model at that tier, its headroom bar, used%, and status. Top-ranked candidate highlighted. Spawn passes `ResolveOptions.Tier`; the resolver picks the actual backend+model. |

### Non-goals (explicitly out of scope)

- **NG1 — Provider scrape rewrite.** Cursor / Antigravity / Claude / Codex
  usage adapters keep their current `Limit` shapes. This work only **projects**
  those rows into scoped `BackendQuota` records (D5).
- **NG2 — Soft mid-session quota hot-swap policy change.** Confirmed hard-limit
  recovery remains the trigger for provider-quota switching. Scoped headroom
  changes eligibility / ranking only; it does not revive the deprecated soft
  rolling-quota handover thresholds.
- **NG3 — Pay-per-use / local backends.** Local and pay-per-use backends stay
  outside subscription headroom routing as today (`AllowPaid`, `IsLocal`).
- **NG4 — OpenAPI / website / skill DoD.** Phase 0 is docs-only. Client and
  public-doc updates land with the implementing phases (see §5), not here.

---

## 3. Canonical scope definitions (D7)

Seed-level tags on every `ModelEntry`. Strings below are the exact `QuotaScope`
/ `BackendQuota.Scope` values.

| Backend | Scope | Models |
|---|---|---|
| **claude** | `session` | all models |
| **codex** | `codex` | all models |
| **antigravity** | `gemini` | `gemini-*` models |
| **antigravity** | `non-gemini` | `claude-*`, `gpt-oss-*` models |
| **cursor** | `api` | `claude-*`, `gpt-*`, `gemini-*`, `glm-*`, `kimi-*` and other non-`cursor-grok` / non-`composer` models |
| **cursor** | `auto` | `auto` model only |
| **cursor** | `included` | `cursor-grok-*`, `composer-*` models |

Rules:

- Blank `QuotaScope` on a `ModelEntry` (custom models, legacy rows before seed
  backfill) resolves to `"default"` at read time (D2).
- Cursor selectors match the triple-bucket brief: included = composer +
  cursor-grok families; auto = exact `["auto"]`; api = everything else on the
  live menu. Do **not** use stale `autoBucketModels` as an included selector.
- Antigravity multi-window rows that share a scope (5-hour + weekly) collapse to
  `min(headroom)` inside `GetModelHeadroom` (D3).

---

## 4. Data model & API (frozen shapes)

Field names below are the JSON wire names; Go casing follows repo convention at
implementation time. Existing fields are marked *(exists)*; new fields **(new)**.

### 4.1 `BackendQuota`

```jsonc
{
  "backend_id":      "cursor",          // (exists)
  "scope":           "auto",            // (new) canonical scope string; migrate missing → "default"
  "window_type":     "monthly",         // (exists)
  "window_duration": 2592000000000000,  // (exists) ns, as today
  "quota_limit":     100,               // (exists)
  "used_amount":     45,                // (exists)
  "last_reset":      "…",               // (exists)
  "next_reset":      "…",               // (exists)
  "limited_until":   "…",               // (exists) per-scope hard-limit recovery (D6)
  "events":          [ … ],             // (exists)
  "updated_at":      "…"                // (exists)
}
```

- **Store key:** `backendID + ":" + scope` (D1). Migration on first read of a
  legacy key `backendID` with empty/missing `scope`: set `Scope = "default"`,
  rewrite to key `backendID:default`, delete the old key.
- **`GetHeadroom(backendID, now)`:** `min` over every scoped record for that
  backend (and still honour backend-row `LimitedUntil` for backward compat).
- **`GetModelHeadroom(backendID, modelID, now)` (new, D3):** resolve
  `ModelEntry.QuotaScope` (blank → `"default"`), load matching quota record(s),
  return headroom / used / limit / limited. Multi-window same-scope → `min`.

### 4.2 `ModelEntry`

```jsonc
{
  "backend_id":   "cursor",                 // (exists)
  "model_id":     "cursor-grok-4.6-high-fast", // (exists)
  "tier":         "tier-1",                 // (exists)
  "display_name": "…",                      // (exists)
  "enabled":      true,                     // (exists)
  "auto_assign":  true,                     // (exists)
  "is_custom":    false,                    // (exists)
  "quota_scope":  "included"                // (new) blank → "default" (D2, D7)
}
```

Seed backfill tags every default model per §3. Custom models may omit
`quota_scope` and fall through to `"default"`.

### 4.3 Resolver (D4, D6)

- `EvaluateCandidates` calls `GetModelHeadroom(m.BackendID, m.ModelID, now)`.
- Candidate `Limited` / reject reason reflects the **scope** limit.
- Check order for cooldown: scoped `BackendQuota.LimitedUntil` first; only if
  every scope is limited (or the backend row is stamped under D6) does the
  backend-wide `Backend.LimitedUntil` block residual callers.
- Ranking / round-robin unchanged aside from the headroom source.

### 4.4 QuotaSync path (D5)

```
Snapshot (backendusage.Service) ──success──► for each Limit row:
                                               SetQuota(BackendQuota{
                                                 BackendID: backend,
                                                 Scope:     limit.Scope,  // already correct
                                                 …derived used/limit/reset…
                                               })
```

- Runs after every successful Snapshot and once on daemon startup.
- One store record per returned `Limit` (scope string from the provider).
- Does not invent scopes the provider did not return; missing scopes retain
  last-known store state until the next successful sync refreshes them.

### 4.5 TUI spawn (D8)

- Replace the per-backend CLI dropdown with a **tier selector**: `auto` |
  `tier-1` | `tier-2` | `tier-3`.
- Below it: live candidate table of AutoAssign models at that tier — headroom
  bar, used%, status; top-ranked candidate highlighted.
- Spawn passes `ResolveOptions.Tier` (existing field); resolver selects
  backend+model. Explicit preferred backend/model paths remain for CLI/MCP
  pins.

---

## 5. Phase outline (implementation; not this PR)

Phase 0 is **this document only**. Later phases build against §2–§4:

| Phase | Deliverable | Locked decisions |
|---|---|---|
| **P0 — Spec freeze** | This doc. No production code. | — |
| **P1 — Store shapes + migration** | `BackendQuota.Scope`, key `backend:scope`, first-read migration to `"default"`; `ModelEntry.QuotaScope`; seed tags per §3; `GetModelHeadroom`; `GetHeadroom` = min-across-scopes. Unit tests for migration + multi-window min. | D1, D2, D3, D7 |
| **P2 — Resolver + LimitedUntil** | `EvaluateCandidates` → `GetModelHeadroom`; scoped-first `LimitedUntil`; `Backend.LimitedUntil` only when all scopes limited. Resolver / recovery tests with Cursor api-exhausted / auto-remaining fixture. | D4, D6 |
| **P3 — QuotaSync** | Project Snapshot `Limit` rows into scoped `BackendQuota` after every successful Snapshot and on daemon startup. | D5 |
| **P4 — TUI spawn** | Tier selector + live candidate table; spawn via `ResolveOptions.Tier`. | D8 |
| **P5 — DoD docs** | README / `docs/` / site / skill / `make gendocs` as affected. Confirm tag with maintainer before any `v*` push. | — |

Each implementing phase opens a PR against `autopilot/per-scope-quota-routing`
(never `main`) and ends with `wd job done --summary '…'` once green.

---

## 6. Out of scope for Phase 0 (this doc)

Phase 0 is a **docs-only spec freeze**. No production code, no schema migration,
no OpenAPI edits, no TUI changes, and no seed edits land in this phase. The
implementation phases that build against this frozen contract are enumerated in
§5.
