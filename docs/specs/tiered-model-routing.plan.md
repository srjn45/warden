# Tiered Model-Based Agent Creation & Quota-Balanced Routing — Implementation Plan

> **Plan Tracking Document**  
> **Status:** In Progress  
> **Target:** Multi-backend model tiering, quota headroom round-robin, and 90% mid-session context handoff.

---

## Progress Overview

- [x] **Prerequisites: Antigravity Warden Skill Setup**
- [x] **Stage 1: Data Model, ScrivaDB Store & Model Catalog**
- [x] **Stage 2: Quota Tracking & Dynamic Weighted Resolver**
- [ ] **Stage 3: Universal Context Dump & Mid-Session Hot-Swap Engine**
- [ ] **Stage 4: Pipeline Integration, CLI & OpenAPI Surfaces**

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
  - [x] Seed Tier-2 models (`claude-3-7-sonnet`, `Claude Sonnet 4.6 (Thinking)` / `Gemini 3.1 Pro`, `sonnet-3.7`, `gpt-4.1`).
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
- [ ] **Task 3.1: Context Extractor** (`internal/handoff/extract.go`)
  - [ ] Extract Goal, Decisions Log, Git Diff State, and Immediate Next Step from `Turn` history.
- [ ] **Task 3.2: Structured Markdown Serializer** (`internal/handoff/serialize.go`)
  - [ ] Write `.warden/handoff-<session-id>.md`.
- [ ] **Task 3.3: Hot-Swap Engine** (`internal/lifecycle/switch.go`)
  - [ ] Retire active CLI, launch successor backend + model, inject context via `AGENTS.md` / system prompt.
- [ ] **Task 3.4: 90% Poller Threshold Trigger** (`internal/lifecycle/poller.go`)
- [ ] **Task 3.5: Hot-Swap Tests** (`internal/lifecycle/switch_test.go`)

---

### Stage 4: Pipeline Integration, CLI & OpenAPI Surfaces
- [ ] **Task 4.1: Pipeline Spec Updates** (`internal/pipeline/`)
  - [ ] Support `role:`, `tier:`, and explicit `backend:` / `model:` in `Job` YAML.
  - [ ] Update built-in pipeline templates.
- [ ] **Task 4.2: CLI Commands** (`internal/cli/`)
  - [ ] `warden models list [--by-tier]`
  - [ ] `warden models tier <backend> <model> <tier>`
  - [ ] `warden role tier list` / `warden role set-tier <role> <tier>`
  - [ ] `warden switch [--backend <id>] [--model <id>] [--tier <tier>]`
- [ ] **Task 4.3: OpenAPI & MCP Tools** (`internal/daemon/`, `internal/mcp/`)
- [ ] **Task 4.4: Full End-to-End Verification & Docs**
