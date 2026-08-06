# Backend Registry & Free-tier Internal Routing — Design Spec

**Date:** 2026-08-06
**Status:** Implemented — shipped to `main` in PR #276 (backend registry: store,
API/MCP, internal-thinking router, autopilot unification, CLI/TUI/web).

This spec designs a first-class **backend registry**: warden detects which agent
CLIs are actually installed on the host, records them in an embedded ScrivaDB
store, and lets the user tier each one (free / subscription / pay-per-use) and
mark one as the default — from a dedicated Backends screen on every client, with
a manual rescan for agents installed later. It then routes warden's **own
internal thinking** (classify / summarize / name / digest / curate / REPL
escalation) strictly to **free** backends, degrading gracefully when none are
available.

---

## 0. Design principles

1. **The DB is the single source of truth** for what backends exist, their tier,
   and which is default. Config (`config.yaml`) stops owning the free/paid
   taxonomy; the existing `autopilot.brain.backends` ladder is migrated into the
   store once and then read from it. There is exactly **one** definition of
   "free" in the system.
2. **Detection is a fact, tiering is a preference.** A rescan only ever changes
   `Installed`/`BinaryPath`/`DetectedAt`. It never touches the user's `Tier`,
   `Default`, or `Enabled` marks — those survive uninstall/reinstall.
3. **Internal thinking never spends.** warden's own offload work runs on the
   **local model** and/or **free-tier** backends only — never a paid backend. The
   local model (Ollama) is itself listed as a backend and is the always-available,
   never-rate-limited floor. A store-configured **mode** picks the shape:
   `local_only` (only the local model) or `free_plus_local` (free cloud backends
   first, local model as the never-limited fallback). Rationale: a powerful host
   can run a good local model and prefer `local_only`; a weak host (poor local
   quality) prefers `free_plus_local` so free cloud tiers do the thinking and the
   local model only backstops when they are exhausted/limited. If neither is
   available the offload degrades gracefully (deterministic slug, skipped
   narration). See §7.
4. **The daemon owns detection and policy; clients are thin.** All four clients
   (CLI, TUI, web, Android) call the same daemon endpoints, so "list", "rescan",
   "set tier", and "set default" behave identically everywhere.

---

## 1. Goal & scope

### In scope

- A ScrivaDB-backed registry of agent backends with per-backend `Installed`,
  `Tier`, `Default`, `Enabled`, and limit state.
- Startup detection + on-demand **rescan** via `exec.LookPath` over the backend
  registry (generalizing the autopilot-only detector that exists today).
- Daemon API: list / rescan / set-tier / set-default.
- A **Backends screen/page** on TUI, web, and Android; `warden backends …`
  subcommands on the CLI; a rescan action on all four.
- The local model (Ollama) surfaced as a first-class `local` backend row, plus a
  store-configured internal-thinking **mode** (`local_only` / `free_plus_local`).
- Free/local-only routing for warden's internal offload paths + REPL escalation.
- Migrating `autopilot.brain.backends` into the store and having autopilot's
  `selectBackend` read tiers from the store.
- Making the user-chosen default override the compile-time `claude` default for
  user agents (where a backend is unspecified).

### Out of scope (named so the first build stays reviewable)

- Auto-guessing a tier from provider pricing. Newly detected backends land in an
  **`unclassified`** state (treated as *not free*) until the user tiers them; we
  do not presume to know whether someone's `codex`/`cursor` is on a free tier.
- Per-model tiering within a backend (tier is per backend id).
- Changing dollar-accounting (`wd spend` / `savings`) — still Claude-specific.
- Android beyond a read + tier + rescan screen (consistent with the Android MVP
  spec being planning-stage).

---

## 2. Concepts

| Term | Definition |
|---|---|
| **Backend** | A registered agent adapter (`internal/agentbackend/backends/*`): `claude`, `aider`, `opencode`, `codex`, `antigravity`, `cursor`, `crush`, `goose`, `terminal`. Identity is its `ID()`; the CLI it needs is `Binary()`. |
| **Local backend** | The local model (Ollama) surfaced as a first-class registry row, id `local`. It is *not* a spawned CLI agent — it is warden's existing `llm.Completer` (`internal/llm/ollama.go`, config `local_llm`). Always tier `local` (a $0, never-limited class), used only for internal thinking (not user coding agents, for now). `Installed` = `local_llm` is configured and reachable. |
| **Installed** | `exec.LookPath(Binary())` succeeds on the daemon host. `terminal` is always "installed" (it is the host shell, not an AI agent) and is never a candidate for internal thinking. `local` is "installed" when its endpoint is configured/reachable. |
| **Tier** | User preference: `free` \| `subscription` \| `pay_per_use` \| `unclassified`, plus the reserved `local` tier for the local backend. Drives internal-thinking eligibility (§7) and autopilot's cost ladder (§8). |
| **Internal-thinking mode** | Store-level setting `local_only` \| `free_plus_local` governing which backends warden uses for its own offload (§7). Default `free_plus_local`. Persisted in ScrivaDB, not config. |
| **Default** | Exactly one backend flagged default; overrides the compile-time `claude` default when a user agent is spawned without an explicit `--backend`. (`local`/`terminal` cannot be the user-agent default.) |
| **Enabled** | User may disable a backend entirely (hidden from pickers, never used internally, rejected on explicit spawn with a clear error). Default `true`. |
| **LimitedUntil** | Timestamp until which a backend is considered rate/spend-limited and skipped for internal thinking. Zero = OK. Set on observed limit, cleared on expiry. The `local` backend can **never** be limited (its `LimitedUntil` is always zero). |

---

## 3. Data model — new ScrivaDB store `internal/backendstore`

Follows the modern `internal/schedule/store.go` template (fresh store — **no**
legacy JSON import, so none of the sentinel/`importLegacy` machinery). DB dir:
`filepath.Join(cfg.DataDir, "backends")` → `~/.warden/backends`.

```go
package backendstore

type Backend struct {
    ID           string    `json:"id"`            // "claude", "local", … — stable ScrivaDB key
    Installed    bool      `json:"installed"`
    BinaryPath   string    `json:"binary_path"`   // resolved LookPath (empty for local)
    DetectedAt   time.Time `json:"detected_at"`
    Tier         string    `json:"tier"`          // free|subscription|pay_per_use|unclassified|local
    Default      bool      `json:"default"`
    Enabled      bool      `json:"enabled"`
    IsLocal      bool      `json:"is_local"`      // the local-model row (uses llm.Completer, never limited, never a user-agent default)
    LimitedUntil time.Time `json:"limited_until,omitempty"` // always zero when IsLocal
}

// Settings is a singleton record (reserved key "__settings__" in the same
// collection) holding store-level policy that is not per-backend.
type Settings struct {
    ID                   string `json:"id"`                     // "__settings__"
    InternalThinkingMode string `json:"internal_thinking_mode"` // local_only | free_plus_local (default free_plus_local)
    AllowPaidAutopilot   bool   `json:"allow_paid_autopilot"`   // migrated from autopilot.allow_pay_per_use (§8)
}
```

Store shape (mirrors `schedule.Store`):

```go
type Store struct {
    mu  sync.Mutex
    db  *scriva.DB
    col *engine.Collection
}

func NewStore(dir string) (*Store, error) // MkdirAll(0700) → scriva.Open(dir, WithSyncMode(SyncModeNone)) → Collection("backends")

func (s *Store) List() ([]Backend, error)                     // col.Scan(query.MatchAll), sort by ID
func (s *Store) Get(id string) (Backend, error)               // GetByKey → ErrNotFound
func (s *Store) Upsert(b Backend) error                       // Insert-or-Update by key
func (s *Store) SetTier(id, tier string) error                // RMW
func (s *Store) SetEnabled(id string, on bool) error          // RMW
func (s *Store) SetDefault(id string) error                   // clears others' Default, sets this one (single writer under s.mu); rejects local/terminal
func (s *Store) Default() (Backend, bool, error)              // the flagged default, if any
func (s *Store) Settings() (Settings, error)                  // reserved "__settings__" record, defaults applied
func (s *Store) SetThinkingMode(mode string) error            // local_only | free_plus_local
func (s *Store) Close() error
```

The reserved `__settings__` key is excluded from `List()` (filter it out of the
`Scan` result) so it never appears as a backend row.

Records are tiny, so the ScrivaDB 16 MiB per-record limit is a non-issue.
`SetDefault` flips the single-default invariant under `s.mu` (read all → clear
the old default → set the new one); a unique secondary index is unnecessary
given the mutex, but `WithUniqueIndex` on a `default` sentinel is a possible
belt-and-suspenders.

**Wiring** (`internal/cli/daemon.go`, alongside the other stores ~line 278):

```go
backendStore, err := backendstore.NewStore(filepath.Join(cfg.DataDir, "backends"))
// … err handling …
defer backendStore.Close()
srv.SetBackends(backendStore) // new setter on daemon.Server, field like cstore/mbox
```

---

## 4. Detection

Generalize the existing autopilot-only detector
(`internal/cli/autopilot.go:detectInstalledBackends`) into a shared function so
there is one detector for the whole product:

```go
package agentbackend

// Detected reports one backend's presence on the host.
type Detected struct{ ID, Binary, Path string; Installed bool }

func Detect() []Detected {
    out := make([]Detected, 0, len(IDs()))
    for _, id := range IDs() {
        b, _ := Get(id)
        p, err := exec.LookPath(b.Binary())
        out = append(out, Detected{ID: id, Binary: b.Binary(), Path: p, Installed: err == nil})
    }
    return out
}
```

A **reconcile** step folds detection into the store, preserving preferences:

```go
// internal/backendstore (or a small service in daemon)
func Reconcile(store *Store, det []agentbackend.Detected, now time.Time) error {
    for _, d := range det {
        b, err := store.Get(d.ID)
        if errors.Is(err, ErrNotFound) {
            b = Backend{ID: d.ID, Tier: "unclassified", Enabled: true} // first sight
        }
        b.Installed, b.BinaryPath, b.DetectedAt = d.Installed, d.Path, now
        store.Upsert(b)
    }
    return nil
}
```

- **On daemon start:** `Reconcile(store, agentbackend.Detect(), now)` then run the
  one-time config migration (§8).
- **On rescan:** identical `Reconcile` call; returns the fresh list to the caller.
- `terminal` is force-`Installed=true` and marked non-AI so it is never an
  internal-thinking candidate and never auto-defaulted.
- **The `local` row** is reconciled separately from PATH detection, from
  `cfg.LocalLLM`: `IsLocal=true`, `Tier="local"`, `Installed` = endpoint
  configured (and optionally a cheap reachability probe), `LimitedUntil` forced
  zero. It has no `Binary()`; `BinaryPath` stays empty. If `local_llm` is not
  configured it still appears as a row with `Installed=false` so the user can see
  it and knows to configure it.
- Preferences survive: only the detection fields (`Installed`/`BinaryPath`/
  `DetectedAt` for CLI backends; `Installed` for `local`) are written on
  reconcile — `Tier`/`Default`/`Enabled` are never overwritten.

`internal/cli/doctor.go` (which currently hardcodes `claude` in its binary
check) should additionally surface the detected set, so `warden doctor` and the
Backends screen agree.

---

## 5. Tiering, default & first-run seeding

- New backends appear as `unclassified` (⇒ *not free* ⇒ ineligible for internal
  thinking, ineligible for autopilot's free tier). This is deliberate: warden
  never guesses that a paid account is free.
- **First-run seeding from the existing config ladder (§8):** any id already
  listed under `autopilot.brain.backends.{free,subscription,pay_per_use}` is
  seeded into the matching tier, so existing setups keep working. Ids not in the
  ladder start `unclassified`.
- **Default seeding:** if no record has `Default=true` after migration, seed the
  default to `claude` when installed, else the first installed non-`terminal`
  backend, else none. The compile-time `agentbackend.DefaultID = "claude"`
  remains the ultimate fallback when the store has no default.

---

## 6. Daemon API (spec-first)

Per repo convention, **edit `openapi.yaml` then `make generate`** — never
hand-write handlers/DTOs. New routes (none are streaming, so no
`oapi/config.yaml` exclude entries needed):

| Method & path | Purpose | Body / result |
|---|---|---|
| `GET /api/v1/backends` | List registry + settings | `{backends: [Backend…], settings: Settings}` |
| `POST /api/v1/backends/rescan` | Re-detect + reconcile | returns the updated `{backends: […], settings}` |
| `PATCH /api/v1/backends/{id}` | Set tier and/or enabled | `{tier?, enabled?}` → updated `Backend` |
| `PUT /api/v1/backends/default` | Set the default backend | `{id}` → updated list |
| `PUT /api/v1/backends/thinking-mode` | Set internal-thinking mode | `{mode}` (`local_only`\|`free_plus_local`) → updated `Settings` |

`Backend`/`Settings` DTOs mirror §3. Validation: backend `tier ∈
{free,subscription,pay_per_use,unclassified}` (the reserved `local` tier is
system-set, not user-assignable); `mode ∈ {local_only,free_plus_local}`;
`PUT default` rejects an uninstalled/disabled/`terminal`/`local` id with a 4xx and
a clear message.

MCP parity (memory: new tools in `internal/mcp/tools_extra.go`): `list_backends`,
`rescan_backends`, `set_backend_tier`, `set_default_backend`,
`set_thinking_mode` — so agents and the skill can drive the registry too.

---

## 7. Internal free-only routing (core requirement)

Today warden's internal offload is *local Ollama first → hardcoded
`agentbackend.Default()` (= `claude`)*, at these sites:

- `internal/lifecycle/lifecycle.go` — `runClaudeP` used by `Classify`,
  `Summarize`, `GenerateName`.
- `internal/digest/narrator.go` — `ClaudeNarrator.Summarize`.
- `internal/curate/propose.go` — `LLMProposer.Propose` fallback.
- `internal/repl/tier_wiring.go` — literal `exec.CommandContext(ctx,"claude","-p",…)`.

Introduce one policy object, injected where `Lifecycle`/REPL/curate are
constructed. It yields an ordered candidate list; each candidate is either the
**local completer** or a **free CLI backend's headless command**:

```go
type InternalRouter struct {
    store *backendstore.Store
    local llm.Completer   // the Ollama completer (may be nil/unconfigured)
    now   func() time.Time
}

type Candidate struct {
    Local   bool               // use r.local (llm.Completer)
    Backend agentbackend.Backend // else this free CLI backend's HeadlessCmd
}

// Candidates returns the ordered attempt list per the store's thinking mode.
// Empty ⇒ internal thinking must degrade (deterministic behavior). Never
// includes a paid/subscription/pay-per-use or disabled backend.
func (r *InternalRouter) Candidates() []Candidate
```

**Mode drives the candidate order** (a free CLI backend is eligible only if
`Installed && Enabled && Tier=="free" && LimitedUntil.Before(now)`; the local
candidate is eligible only if `local` is configured/reachable):

- **`local_only`** → `[local]`. Only the local model does the thinking; free
  cloud backends are never used. (Powerful-host setting.)
- **`free_plus_local`** → `[free backends (default-first, then stable id), …,
  local]`. Free cloud tiers do the thinking; the **local model is the last,
  never-limited fallback** so internal thinking almost never fully degrades.
  (Weak-host setting — free cloud quality beats the weak local model, but local
  guarantees a floor when every free backend is rate-limited.)

Call sequence at every offload site becomes:

1. Walk `Candidates()` in order. For a free-CLI candidate, run its headless
   command; **on a rate-limit/spend signal**, stamp `LimitedUntil` and continue
   to the next candidate. For the local candidate, call `r.local` (it can never
   be limited, so it terminates the walk).
2. If the list is empty or every candidate fails: **degrade gracefully** —
   `GenerateName` → deterministic slug (already its behavior), `Summarize`/digest
   narration → skip, `Classify` → default bucket, curate → no proposal ($0). No
   paid call, ever.

This deletes the hardcoded-`claude` fallback from all four sites. REPL escalation
(`tier_wiring.go`) routes through the same `InternalRouter` instead of a literal
`claude`. Note the local model is no longer a hardcoded "always first" step — in
`local_only` it is the only step, and in `free_plus_local` it is intentionally
the **last** step (free cloud preferred), matching the powerful-vs-weak-host
rationale.

**Limit signal source:** reuse the existing rate-limit detection warden already
performs on agents (the autopilot `selectBackend` path already tracks
rate-limited backends). For one-shot headless calls we additionally treat a
non-zero-exit-with-limit-pattern (or spend-cap output) as the trigger to stamp
`LimitedUntil`. Expiry is a short TTL (config, e.g. default 15m) after which the
backend is retried.

---

## 8. Autopilot reconciliation (unify into the DB)

- `autopilot.brain.backends.{free,subscription,pay_per_use}` and
  `allow_pay_per_use` are **migrated once** into the store on first boot after
  upgrade: each listed id gets the matching tier; a `.backends-migrated`
  sentinel (or a config version bump) prevents re-import so later user edits in
  the store are authoritative.
- `internal/autopilot/select.go:selectBackend` changes its source: instead of
  reading the config ladder, it reads tiers from the store — `free` →
  `subscription` → `pay_per_use` (last gated by a store/daemon-level
  "allow paid" flag preserved from `allow_pay_per_use`). Rate-limited/excluded
  handling is unchanged.
- The config keys are **deprecated** (kept loading with a one-time deprecation
  warning per the existing `flatKeyGroups` deprecation pattern) and documented as
  superseded by the registry. `AutopilotBrainBackends` accessor returns the
  store-derived ladder.
- Net effect: one definition of "free" shared by internal thinking and autopilot.

---

## 9. Clients — Backends screen + rescan everywhere

All are thin callers of §6.

**CLI** (`internal/cli/`, new cobra command group; then `make gendocs`):

```
warden backends list                 # table incl. the local row; id, installed, tier, default, enabled, limited
warden backends rescan               # re-detect, print updated table
warden backends tier <id> <tier>     # free|subscription|pay_per_use|unclassified
warden backends default <id>         # set default (rejects local/terminal)
warden backends enable|disable <id>
warden backends thinking-mode <mode> # local_only | free_plus_local
```

**TUI** (`internal/tui/`): a **Backends page** — list of detected backends
(including the `local` row) with columns (installed ✓, tier, default ●, enabled);
keys to cycle tier, set default, toggle enabled, and `r` to rescan; a header
control to toggle the internal-thinking mode. Reachable from the main nav
alongside the existing panes.

**Web** (the app UI served by the daemon): a **Backends settings page** — same
table with tier dropdowns, a default radio, enable toggles, a
**thinking-mode** selector (Local only / Free + local), and a **Rescan** button.

**Android** (`docs/specs/2026-08-05-android-app-design.md` is planning-stage): add
a **Backends screen** to that spec — read the list (incl. local), tier/default/
enable, the thinking-mode selector, and a rescan button — all via the new REST
endpoints (no daemon changes beyond §6).

---

## 10. Config changes & migration

- **Removed as source of truth (deprecated, still parsed):**
  `autopilot.brain.backends.*`, `autopilot.brain.allow_pay_per_use` — migrated to
  the store (§8).
- **New (small):** an internal-router limit TTL, e.g.
  `backends.limit_retry` (duration, default `15m`) under a new `backends` config
  block — operator-tunable, not per-backend state (state is in the DB).
- **Unchanged:** `local_llm` (URL/model/timeout) still configures the local model
  in `config.yaml`; the registry's `local` row *reflects* that config but does not
  replace it. The **thinking mode** itself lives in the DB (`Settings`), per the
  decision that this is user preference, not operator config.
- The local model is never eligible as a *user-agent* backend via this work; it
  remains internal-thinking-only. (Making `local` spawnable for coding agents is a
  possible future item, out of scope here.)
- Migration is one-time and guarded (sentinel / config version), consistent with
  `migrateFlatToNamespaced` / `Reconcile` in `internal/config`.

---

## 11. Edge cases

- **No free backend installed / all limited:** in `free_plus_local` the local
  model backstops; if the local model is also unconfigured, offloads degrade
  (deterministic name, no narration). warden stays fully functional — these are
  cosmetic/assistive paths.
- **`local_only` with no local model configured:** internal thinking degrades
  entirely (all offloads deterministic/skipped); the Backends page shows the
  `local` row as not-installed so the cause is visible. warden never silently
  falls back to a paid backend.
- **Mode = `local_only` on a weak host:** allowed but discouraged; purely the
  user's call. No quality gating — warden respects the setting.
- **User disables the default:** `SetDefault` refuses to leave the registry with a
  disabled/uninstalled default; setting default requires an installed+enabled id.
- **Backend uninstalled between rescans:** record kept, `Installed=false`; it is
  skipped everywhere until reinstalled; tier/default marks preserved.
- **`terminal`:** never internal-thinking-eligible, never auto-defaulted; shown in
  the list as installed for completeness.
- **Concurrent edits from two clients:** `s.mu` + ScrivaDB per-collection locking
  serialize writes; `SetDefault` is atomic under the mutex.

---

## 12. Build stages (proposed warden pipeline)

Each stage is a short-lived agent with a bounded context; stages are mostly
sequential with a couple of parallel client stages at the end.

1. **Store + detection** — `internal/backendstore`, shared `agentbackend.Detect`,
   `Reconcile`, unit tests. Daemon wiring + `SetBackends`.
2. **Daemon API** — `openapi.yaml` routes + `make generate` + handlers + MCP
   tools. (Depends on 1.)
3. **Internal router** — `InternalRouter`, rewire the four offload sites + REPL
   escalation to free-only with graceful degrade. (Depends on 1.)
4. **Autopilot unify** — migrate the config ladder into the store; point
   `selectBackend` at the store; deprecate config keys. (Depends on 1–3.)
5. **CLI** — `warden backends …` + `make gendocs`. (Depends on 2.)
6. **TUI page** — Backends pane + rescan. (Depends on 2.) — parallel with 5/7.
7. **Web page** — Backends settings + rescan. (Depends on 2.) — parallel with 5/6.
8. **Android spec** — extend the Android design doc with the Backends screen.
9. **Docs & DoD** — README, `docs/FEATURES.md`, `FEATURES.md` matrix,
   `docs/USAGE.md`, site guide + `reference/cli.md` (generated), skill, and a
   tag/release per the DoD checklist.

---

## 13. Definition-of-Done checklist (per CLAUDE.md)

- **Tag & release:** one tag for the feature (minor bump — it is a sizable
  feature); confirm with maintainer before pushing the `v*` tag.
- **Docs:** `README.md`; `docs/FEATURES.md` + root `FEATURES.md` matrix;
  `docs/USAGE.md`; website `guides/` + `reference/cli.md` (via `make gendocs`);
  `skills/warden/` if agent-facing drive changes.
- **CLI help/manual:** cobra `Use`/`Short`/`Long`/flags for the new `backends`
  group; `make gendocs` + commit; `make gendocs-check` stays green.

---

## Appendix — key source anchors (as of 2026-08-06)

- Backend registry & interface: `internal/agentbackend/backend.go` (interface
  ~L109), `registry.go` (`DefaultID = "claude"`, `Register`/`Get`/`IDs`),
  adapters in `internal/agentbackend/backends/*`.
- Existing detector (to generalize): `internal/cli/autopilot.go`
  `detectInstalledBackends`.
- Config ladder (to migrate): `internal/config/config.go` `AutopilotBackends`
  (~L175), `AutopilotBrainBackends` accessor (~L1449).
- Internal offload sites: `internal/lifecycle/lifecycle.go` (`runClaudeP`,
  `Classify`/`Summarize`/`GenerateName`); `internal/digest/narrator.go`;
  `internal/curate/propose.go`; `internal/repl/tier_wiring.go`.
- Store template: `internal/schedule/store.go`; daemon wiring:
  `internal/cli/daemon.go` (~L278); server setters pattern:
  `internal/daemon/server.go`.
- ScrivaDB: `github.com/srjn45/scriva v1.2.1` — `scriva.Open`,
  `db.Collection`, `engine` keyed CRUD, `SyncModeNone`.
