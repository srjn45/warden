# Tiered Model-Based Agent Creation & Quota-Balanced Routing — Implementation Plan

> **Plan Tracking Document**  
> **Status:** Complete  
> **Target:** Multi-backend model tiering, quota headroom round-robin, and 90% mid-session context handoff.

> **First-spawn wiring (tier trio, done).** The one remaining gap after Stage 4 —
> the **initial** `Lifecycle.Spawn` / `SpawnJob` still bypassed the resolver and
> only hot-swaps routed through it — is now closed. Both spawn paths call
> `resolveSpawnTarget`, which routes the initial backend+model through the same
> quota-balanced resolver by `{role, task, tier}` (a pinned backend/model still
> wins; it degrades to request defaults when no resolver is wired, so a first spawn
> never hard-fails). The task registry (`internal/task`) is the canonical task→tier
> source, roles carry real default tiers, and precedence is
> `explicit tier > task tier > role tier > tier-2`. See
> [`docs/specs/agent-roles.md`](agent-roles.md#tier-at-spawn-resolution) for the
> roles-vs-tasks model and an end-to-end test in
> `internal/lifecycle/tier_trio_integration_test.go`.

---

## Progress Overview

- [x] **Prerequisites: Antigravity Warden Skill Setup**
- [x] **Stage 1: Data Model, ScrivaDB Store & Model Catalog**
- [x] **Stage 2: Quota Tracking & Dynamic Weighted Resolver**
- [x] **Stage 3: Universal Context Dump & Mid-Session Hot-Swap Engine**
- [x] **Stage 4: Pipeline Integration, CLI & OpenAPI Surfaces**
- [x] **Tier trio: first-spawn routing** — `Spawn`/`SpawnJob` resolve backend+model via the router; task→tier registry + role default tiers wired

---

## Detailed Task Breakdown

### Prerequisites: Antigravity Warden Skill Setup
- [x] **Prereq 1.1: Register Warden Skill in Antigravity**
  - [x] Workspace level: Link `skills/warden` to `.agents/skills/warden`.
  - [x] Global level: Link `skills/warden` to `~/.gemini/antigravity-cli/skills/warden`.
  - [x] Validate skill frontmatter and progressive disclosure references (`references/agents.md`, `operations.md`, `pipelines.md`, `git-and-checks.md`, `coordination.md`).

### Stage 1: Data Model & ScrivaDB Store (`internal/backendstore/`)
- [x] **Task 1.1: Define Go Types & Constants** (`internal/backendstore/types.go`)
  - [x] Define `ModelTier` enum (`tier-1`, `tier-2`, `tier-3`).
  - [x] Define `ModelEntry` struct (`backend_id`, `model_id`, `tier`, `enabled`, `display_name`).
  - [x] Define `RoleTierMapping` struct (`role_name`, `default_tier`).
  - [x] Define `HandoverSettings` struct (`enabled`, `threshold_percent` = 90, `rolling_quota_threshold` = 90, `context_fill_threshold` = 90, `cooldown_period` = 15m).
- [x] **Task 1.2: Seed Default Catalog & Role Mappings** (`internal/backendstore/seed.go`)
  - [x] Seed Tier-1 models (`claude-opus`, `Claude Opus 4.6 (Thinking)`, `claude-3-opus`, `o1`).
  - [x] Seed Tier-2 models (`sonnet`, `Claude Sonnet 4.6 (Thinking)` / `Gemini 3.1 Pro`, `sonnet-3.7`, `gpt-4.1`).
  - [x] Seed Tier-3 models (`claude-3-5-haiku`, `Gemini 3.5 Flash`, `composer-2.5-fast`, `gpt-4.1-mini`).
  - [x] Seed Role Tier mappings (`analysis` -> `tier-1`, `architecture` -> `tier-1`, `planning` -> `tier-1`, `design` -> `tier-1`, `arch-design-review` -> `tier-1`, `autopilot` -> `tier-1`, `pr-review` -> `tier-1`, `implementation` -> `tier-2`, `debugger` -> `tier-2`, `code-review` -> `tier-2`, `ci-triage` -> `tier-3`).
- [x] **Task 1.3: Store Methods & Migration** (`internal/backendstore/store.go`)
  - [x] Add `ListModels(tierFilter ModelTier) ([]ModelEntry, error)`.
  - [x] Add `SetModelTier(backendID, modelID string, tier ModelTier) error`.
  - [x] Add `GetRoleTier(roleName string) (ModelTier, error)`.
  - [x] Add `SetRoleTier(roleName string, tier ModelTier) error`.
  - [x] Add `GetHandoverSettings() (HandoverSettings, error)`.
  - [x] Add `SetHandoverSettings(s HandoverSettings) error`.
- [x] **Task 1.4: Unit Tests & Verification** (`internal/backendstore/models_test.go`)

---

### Stage 2: Quota Tracking & Dynamic Weighted Resolver (`internal/router/`)
- [x] **Task 2.1: Provider Quota Monitor** (`internal/backendstore/quota.go` or `internal/spend/`)
  - [x] 5-hour rolling token/cost tracker for Claude.
  - [x] Daily quota reset tracker for Antigravity.
  - [x] Monthly / rate-limit monitors for Cursor & Codex.
- [x] **Task 2.2: Weighted Headroom Resolver** (`internal/router/resolver.go`)
  - [x] Resolve candidates by `RoleName` -> `ModelTier`.
  - [x] Calculate headroom $H = 1.0 - (\text{usage} / \text{quota})$.
  - [x] Filter out limited/exhausted backends ($\ge 90\%$).
  - [x] Select highest-headroom candidate (with round-robin tie-breaking).
- [x] **Task 2.3: Unit & Failover Tests** (`internal/router/resolver_test.go`)

---

### Stage 3: Universal Context Dump & Mid-Session Hot-Swap (`internal/handoff/`, `internal/lifecycle/`)
- [x] **Task 3.1: Context Extractor** (`internal/handoff/extract.go`)
  - [x] Extract Goal, Decisions Log, Git Diff State, and Immediate Next Step from `Turn` history.
  - Pure/deterministic `Extract([]agentbackend.Turn) Handoff`; git-diff enriched by the caller.
- [x] **Task 3.2: Structured Markdown Serializer** (`internal/handoff/serialize.go`)
  - [x] Write `.warden/handoff-<session-id>.md` with Goal, Decisions, Modified Files, Next Step, System Context.
- [x] **Task 3.3: Hot-Swap Engine** (`internal/lifecycle/switch.go`)
  - [x] Extract → persist handoff → resolve successor (explicit / tier via `internal/router`) → retire active
    CLI (kill tmux) → launch successor in the SAME worktree → inject context via `AGENTS.md`
    (`ContextInjector`) + a continuation prompt that points the successor at the handoff file.
  - Successor resolution via the narrow `SuccessorResolver` seam (`*router.Resolver` satisfies it).
- [x] **Task 3.4: 90% Poller Threshold Trigger** (`internal/lifecycle/poller.go`, `internal/poller/context.go`)
  - [x] Pure `DecideHotSwap(ThresholdInput) HotSwapSignal` policy (context-fill OR provider-quota ≥ 90%,
    with cooldown + unknown-measurement guards) in `internal/lifecycle/poller.go`.
  - [x] Thin, inert-by-default signal wired into the poller (`HandoverEnabled` + `OnHotSwap`), edge-triggered
    once per critical-context episode; the daemon backs `OnHotSwap` with `DecideHotSwap` + `HotSwap`.
- [x] **Task 3.5: Hot-Swap Tests** (`internal/handoff/*_test.go`, `internal/lifecycle/switch_test.go`,
  `internal/lifecycle/poller_test.go`, `internal/poller/hotswap_test.go`)

> **Stage-4 wiring note:** `Lifecycle.Resolver` and the poller's `HandoverEnabled`/`OnHotSwap` seams are
> defined and unit-tested but NOT yet wired by the daemon — that (plus the CLI `warden switch` / MCP surface)
> lands in Stage 4.

---

### Stage 4: Pipeline Integration, CLI & OpenAPI Surfaces
- [x] **Task 4.1: Pipeline Spec Updates** (`internal/pipeline/`)
  - [x] Support `role:`, `tier:`, and explicit `backend:` / `model:` in `Job` YAML.
  - [x] Update built-in pipeline templates.
- [x] **Task 4.2: CLI Commands & Daemon Wiring** (`internal/cli/`, `internal/daemon/`)
  - [x] `warden models list [--by-tier]`
  - [x] `warden models tier <backend> <model> <tier>`
  - [x] `warden role tier list` / `warden role set-tier <role> <tier>`
  - [x] `warden switch [--backend <id>] [--model <id>] [--tier <tier>]`
  - [x] Daemon wiring: `Lifecycle.Resolver = router.NewResolver(d.backendStore)`, poller `HandoverEnabled` and `OnHotSwap` hooks wired to `DecideHotSwap` and `HotSwap`.
  - [x] **First-spawn wiring (tier trio):** `Spawn` and `SpawnJob` route the *initial* backend+model through `resolveSpawnTarget` → `Resolver.Resolve({role, task, tier})`, with a pinned backend/model taking precedence and graceful degradation when no resolver is wired. `--tier` / `--task` added to `warden start` and the REST spawn body (`tier` / `task`); `tier:` (with `role:`) on pipeline jobs — the Job spec has no `task:`.
- [x] **Task 4.3: OpenAPI & MCP Tools** (`internal/daemon/`, `internal/mcp/`)
  - [x] Expose REST endpoints (`GET /models`, `PUT /models/{backend}/{model}/tier`, `GET /roles/tiers`, `PUT /roles/tiers/{role}`, `POST /sessions/{id}/switch`, `GET /handover/settings`, `PUT /handover/settings`).
  - [x] Regenerate OpenAPI code via `make generate` (`internal/daemon/oapi/api.gen.go`).
  - [x] Register MCP tools (`list_models`, `set_model_tier`, `list_role_tiers`, `set_role_tier`, `switch_agent`, `get_handover_settings`, `set_handover_settings`).
  - [x] Comprehensive client, daemon, and MCP tests.
- [x] **Task 4.4: Full End-to-End Verification & Docs**
  - [x] Full test verification (`make verify-fast`, `go test ./...`, `npm test`, `astro build`).
  - [x] CLI reference docs generated and synced (`site/src/content/docs/reference/cli.md`).
  - [x] All 4 stages complete and verified on `main`. (Release tag `v*` pending maintainer confirmation).
