# Warden Future Scope & In-Flight Audit

**Date:** 2026-08-28
**Status:** Historical planning audit; reconciled with `main` through PR #375
**Author:** Srajan (with Claude planner)

> **Completion/supersession notice (2026-08-31).** The tier trio (#1–#3), Project
> Groups (#5), and Open Project/project-centric cockpit (#8) described below have
> shipped. Their old “resume here” passages are preserved as historical design
> context but are explicitly marked superseded. The session-store/TUI stability
> plan is separate and is **beginning now, not complete**; see
> [`../2026-08-30-session-store-tui-flakiness-fix-plan.md`](../2026-08-30-session-store-tui-flakiness-fix-plan.md).

This document originally did two things:

1. **Audited the then-current state** of ten proposed features — because several were
   started and left mid-way (session limits), and it was unclear which are
   half-baked. For each, it records the exact **"resume here"** point.
2. **Lays out the future scope** as a layered architecture with a sequenced
   roadmap, dependencies, and the open decisions that still need a call.

> **Scope note.** This is now a historical planning doc. Shipped behaviour is
> catalogued in [`FEATURES.md`](../FEATURES.md); the older running to-do list is
> [`FUTURE_ENHANCEMENTS.md`](../FUTURE_ENHANCEMENTS.md), which was reconciled in
> the same 2026-08-31 audit. New shipped behavior belongs in FEATURES.md rather
> than the forward-looking roadmap.

---

## 0. TL;DR — status of the ten features

| # | Feature | Status | One-line "resume here" |
|---|---------|--------|------------------------|
| 1 | Roles → **behaviors & tasks** | ✅ **Done** | Shipped: task is persisted separately and is the canonical task→tier source |
| 2 | **Model tiers** across backends → start agent | ✅ **Done** | Shipped: initial direct and pipeline spawn route through the resolver |
| 3 | **Pre-assign tiers** to roles (default + override) | ✅ **Done** | Shipped: explicit tier/task and role defaults use the unified resolver |
| 4 | **Auto-handover** near session limit | ✅ **Done + polished** | Hard-limit path reconciled to hot-swap + proactive quota tracking landed |
| 5 | **Project Groups** | ✅ **Done** | Shipped in PRs #363–#370; old collaboration specs removed |
| 6 | **Remote access via warden-hub** | 🟠 Wire protocol only | Build the daemon `internal/relay` connector + the hub server MVP |
| 7 | **Warden cluster** (multi-machine) | 🔴 Not started | Blocked on #6 |
| 8 | **Open project** (local / clone GH·GL / new) | ✅ **Done** | Shipped with persisted projects and the project-centric TUI (Phases 1–5) |
| 9 | **Warden-teams** (shared knowledge) | 🔴 Not started, no spec | Blocked on #6; builds on shipped project-memory |
| 10 | **Generic agent TUI CLI** (spaiSH/`spai-cli`) | 🔴 External / not tracked here | No warden-side implementation; add only a client seam when required |
| 11 | **Backend hardening** (Tier-C → Tier-A) | 🟡 Partial | Finish transcript, usage, review, and prompt-seeding adapters |
| 12 | **Recover / resume on another backend** | 🟡 Engine shipped, product workflow incomplete | Promote `warden switch` into a resilient top-level recovery capability |

**Legend:** ✅ done · 🟢 built, one wire missing · 🟡 in progress on `main` ·
🟠 partial / spec-only · 🔴 not started.

**Current headline:** #1–#5 and #8 are shipped. #6 remains the main unfinished
foundation for #7/#9; #10 remains external; #11/#12 retain the incomplete status
described below.

---

## 1. The big picture — four layers

The ten features are **not independent**; they stack. Building them out of order is
what produced the current mess. The dependency spine:

```
  Layer 3  ── multi-developer / multi-machine ──────────────────────────
     #7 Cluster        #9 Teams
        └──────────────┬──────────┘
                       │ (both REQUIRE the hub transport)
  Layer 2  ── the hub (the big unlock) ─────────────────────────────────
     #6 warden-hub  (remote access, native mTLS E2E, relay)
                       │
  Layer 1  ── single dev, single machine, many projects ────────────────
     #1 behaviors/tasks ✅  #5 project groups ✅  #8 open project ✅
     #2 tier-at-spawn ✅    #3 role→tier defaults ✅
                       │
  Layer 0  ── foundation (mostly SHIPPED) ──────────────────────────────
     backend registry · tiered model routing · handover/hot-swap · #4
     explicit cross-backend recovery / resume · #12
     ctx_* blackboard · directed messaging · pipeline engine

  Cross-cutting:  #10 generic agent TUI CLI (spaiSH) — reskins the client
                  surface across all layers; can start anytime after a design.
```

**Reading of the map:** Layers 0 and 1 are done. Layer 2 (the hub) is the single biggest lever —
**#7 and #9 cannot exist without it**, so it gates the entire top of the stack.
#10 is orthogonal and can proceed on its own track.

---

## 2. Layer 0 — foundation (shipped, context only)

These already exist on `main` and everything above builds on them. Not re-planned
here; listed so the dependencies are explicit.

- **Backend registry** (`internal/backendstore`, PR #276) — detects installed CLIs
  via `LookPath`, stores them in an embedded ScrivaDB (`~/.warden/backends`),
  user-tiers each backend by **billing tier** (`free` / `subscription` /
  `pay_per_use` / `unclassified` / `local`), one default. Source of truth for
  routing.
- **Tiered model routing** (`internal/router`, `internal/backendstore`) — a
  separate **model tier** vocabulary (`tier-1/2/3`) over ~17 seeded models, a
  quota-tracker (`internal/backendstore/quota.go`, headroom = `1 − used/limit`),
  and a weighted-headroom `Resolver`.
- **Handover / hot-swap engine** (`internal/lifecycle/switch.go`,
  `internal/poller`) — mid-session context dump + relaunch on a successor
  backend/model. (This *is* feature #4 — see §3.4.)
- **Coordination primitives** — `ctx_*` blackboard (`internal/ctxstore`), directed
  messaging + `wait_for_message` (`internal/mailbox`), the pipeline DAG engine
  (`internal/pipeline`), and cockpit project-grouping (PR #292). Features #5/#7/#9
  reuse these rather than inventing new channels.

> ⚠️ **Two "tier" vocabularies — name them precisely to avoid confusion.**
> **Backend billing tier** (`free`/`subscription`/`pay_per_use`/`local`, *per
> backend*, gates spend + internal-thinking eligibility) is **not** the same as
> **model tier** (`tier-1/2/3`, *per model*, drives role/task → model routing). The
> resolver reads *both*: it filters candidate backends by billing tier, then picks
> a model within the requested model tier. Any future doc/UI should always qualify
> which "tier" it means.

---

## 3. Layer 1 — single-machine features

### 3.1 Feature #1 — break "roles" into **behaviors & tasks**

**What you asked for:** stop overloading "role" to mean both *persona* and *unit of
work*. Split into **behaviors** (how an agent acts) and **tasks** (what it's doing).

**Current state — ✅ done on `main`.** Stages 1–4 shipped; the task registry is
the canonical task→tier source and assigned tasks persist in session state.

**Historical state before completion (Stages 1–3, PRs #307/#308/#309):**
- A **behavior** already exists in all but name: `internal/role` holds embedded-YAML
  personas (`general`, `orchestrator`, `planner`, `worker`, `autopilot`, `brain`)
  = a system-prompt persona + default spawn flags. Stage 2 removed the old
  task-like roles (`implementer`/`reviewer`/`auto-merger` → alias `worker`) and
  added `planner`.
- A **task** registry landed in Stage 1: `internal/task` — 13 tasks, each with a
  `Tier (1/2/3)` and a set of `Roles` allowed to run it (e.g. `analysis`,
  `architecture`, `development`, `code-review`, `release`). Task names line up 1:1
  with `store.Type`, not with role names.
- Stage 3 made the **worktree policy role-driven** (`wantWorktree`): worker/planner/
  autopilot isolate; orchestrator/brain never do.

**Superseded resume guidance (gap now closed):** `internal/task` **was not wired into anything** —
only its own test imports it. Tier selection still keys off the *old* hand-kept
role→tier map in `backendstore/seed.go` (`DefaultRoleTiers`, keyed by
`analysis`/`implementation`/`code-review`…), whose keys are **task names masquerading
as role names**. The refactor's finish line:
1. Make `router.DetermineTargetTier` / `DefaultRoleTiers` consume `internal/task.Tier`
   as the source of a task's default tier, instead of the string map.
2. Give spawn a first-class **task** dimension (at the time only `Type` carried it) and
   let **behavior (role) + task** be set independently.
3. Update `docs/specs/agent-roles.md` — it still says "exactly five roles" and
   predates the whole split.

**Design direction:** land on the vocabulary explicitly —
- **Behavior** = the current `role` (persona + defaults). Rename in docs/UI even if
  the Go type stays `role` for now.
- **Task** = `internal/task` entry (tier + eligible behaviors). An agent is
  `(behavior, task)`; task supplies the *default* tier, behavior supplies the
  *persona + worktree/permission policy*.

**Decided (2026-08-28):** **keep the name `role`** as the wire word for behavior —
do **not** rename the package/CLI/MCP/API surface. The split is: `role` stays the
persona+defaults concept; **`task` becomes the separate first-class dimension**
(the thing that gets split *out* of the old overloaded "role"). So the work is
purely additive — introduce the task dimension and make it the tier source; the
existing `role` surface is untouched.

**Open question (minor):** should users be able to define **custom tasks** (they
can't define roles)? Tasks are pure data (tier + eligible-role list), so this is a
cheap natural extension — defer until there's demand.

---

### 3.2 Feature #2 — map models into 3 tiers, start an agent by tier

**What you asked for:** across all backends, classify models into 3 tiers; start an
agent by asking for a tier (warden picks the concrete backend+model).

**Current state — ✅ done.** Initial direct and pipeline spawns now route through
the quota-balanced resolver; explicit backend/model pins retain precedence.

**Historical state before completion (Stages 1–4, PRs #300/#303/#304/#305):** the 3-tier catalog, quota headroom tracking, and the
weighted-headroom `Resolver` (`ResolveOptions{Role, Tier, …} → Resolution`) all
exist and are surfaced via CLI (`warden models …`), MCP (`list_models`,
`set_model_tier`), and REST.

**Superseded resume guidance (wire now shipped):** the resolver was wired **only into
mid-session hot-swap**, not into first-spawn.
- `SpawnRequest` (`lifecycle.go`) has `Backend`/`Model`/`Role` but **no `Tier`
  field**.
- `JobSpawnRequest` *has* a `Tier`, and pipeline jobs carry `role/tier/backend/model`
  (#302) — but `SpawnJob` sets `Backend`/`Model` directly and **never resolves
  `req.Tier`** through the router.
- The daemon spawn path applies only the registry's *default backend*, not tier
  routing.

**So the finish line is small and precise:** add `Tier` to `SpawnRequest`, and route
initial spawn through `router.Resolver` exactly the way `HotSwap`'s
`resolveSuccessor` already does. That single wire delivers "start an agent by tier"
for direct spawns *and* pipeline jobs.

---

### 3.3 Feature #3 — pre-assign tiers to behaviors/tasks (default, user-overridable)

**Current state — ✅ done.** Task defaults, role overrides, and explicit tier
selection feed the unified initial-spawn resolver.

**Historical state before completion:** `RoleTierMapping` + `DefaultRoleTiers` seed
defaults; `DetermineTargetTier` implements the precedence **explicit tier > role/
task default > global default (tier-2)**; `set_role_tier` lets the user override.

**Superseded resume guidance (both items shipped):**
1. Same **first-spawn wiring** as #2 (defaults are computed but not applied at
   spawn).
2. **Re-key the defaults off tasks, not roles** (folds into #1). Today the default
   map is keyed by task-like names; once `internal/task` is the source of tier,
   `DefaultRoleTiers` becomes "task → default tier" and the role→tier override
   becomes a genuine per-behavior override on top.

**Net:** #1, #2, #3 are one coherent piece of work with a shared finish line
(**wire tier resolution into spawn; make tasks the tier source**). Recommend doing
them as a single 3–4 job pipeline rather than three separate efforts.

---

### 3.4 Feature #4 — auto-handover near session limit

**Current state — ✅ done, end-to-end.** This is the most complete of the ten.
- Policy: `DecideHotSwap` fires when enabled + cooldown elapsed + (context-fill ≥
  threshold OR quota-used ≥ threshold); defaults 90% / 15m cooldown.
- Engine: `Lifecycle.HotSwap` extracts a handoff, writes `.warden/handoff-<id>.md`,
  resolves a successor via the resolver, relaunches in the same worktree with a
  continuation prompt + `AGENTS.md` injection.
- Wiring: `internal/cli/daemon.go` feeds live tokens + headroom into `DecideHotSwap`
  and calls `HotSwap` on the poller edge. Fully live.
- ~~A *separate* older path (`internal/daemon/ratelimit.go`) handles hard rate-limit
  banners (parse reset time → schedule resume → un-pause pane).~~ **Now reconciled
  (see polish below):** a hard limit hot-swaps when handover is enabled instead of
  only parking the agent.

**Polish delivered:**
- ✅ **Reconciled the two paths.** `RateLimitScheduler.OnHardLimit` (wired in
  `internal/cli/daemon.go`) fires on the transition INTO `StatusRateLimited`, BEFORE
  the resume schedule: when handover is enabled it marks the exhausted backend
  limited in the registry (so the router excludes it), hot-swaps to a fresh backend
  in the same worktree, clears the pending resume timer/limit, and emits a
  `rate-limit-hotswap` event. A false return (handover off, or no eligible
  successor) falls through to the classic pause-and-resume, so the two paths never
  double-fire.
- ✅ **Proactive quota tracking.** `internal/daemon/quota_recorder.go` samples each
  live agent's cumulative billed tokens (parsed from the transcript warden already
  reads for spend/context — no headless `/usage` spawn) and records the per-agent
  delta into `backendstore.RecordQuotaUsage` on a 60s tick, seeding a baseline on
  first sight so a restart never back-fills a spike. This keeps `GetHeadroom` current
  so the *soft* `DecideHotSwap` quota arm can retire an agent BEFORE a hard limit.

**Still optional:**
- Surface handover events more visibly in TUI/web (they happen somewhat silently).
- Consider a "handover budget" so a thrashing backend doesn't ping-pong.
- Align per-backend quota units (cursor's request-denominated budget vs. the
  token-denominated windows the recorder feeds).

---

### 3.5 Feature #5 — collaboration groups (orchestrators join/leave)

**What you asked for:** the per-project orchestrator from each project can join/leave
named **groups** where orchestrators discover and collaborate with each other.

**Current state — ✅ done on `main`.** The project-group store/API/TUI labels,
orchestrator auto-spawn and wakeup, peer context, and delegation ergonomics shipped
in PRs #363–#368; obsolete specs were removed in PR #370.

**Historical pre-implementation audit (superseded):**

The original specs (removed by PR #370 after the replacement shipped) were:
- `docs/specs/2026-08-26-collaboration-groups.md` (design)
- `docs/specs/2026-08-26-collaboration-groups-impl.md` (8-job pipeline U-B +
  monitoring U-A + TUI U-C)

Design in one breath: a group is a **thin roster + introduction broker**, *not* a new
channel — peers keep using the existing directed-message bus + `ctx_*` blackboard.
Project identity = **normalized canonical git remote URL** (two worktrees of one repo
= one project; local-only repos get a non-portable `local:` key). One orchestrator
per project; join switches the agent to the `orchestrator` behavior, resolves a
project summary, writes the roster entry, and **brokers introductions both
directions with templated messages (zero agent tokens)**. Leave is soft; terminate
is the hard path with a confirmation gate.

**Why it feels confusing (the actual problem):** the impl plan's dependency chain is
**B1 groupstore → B2 project-key → B3 join/leave → B4 introductions**, then B5/B6/B7
hang off B3. Your branches are:

| Job | What it is | Branch? |
|-----|-----------|---------|
| **B1** groupstore | foundation | ❌ none |
| **B2** git-remote project-key | foundation | ❌ none |
| **B3** join/leave core | foundation | ❌ none |
| **B4** brokered introductions | foundation | ❌ none |
| B5 project summary | depends on B4 | ✅ `feat/stage-b5-project-summary` |
| B6 leave/terminate | depends on B3 | ✅ `feat/stage-b6-leave-terminate` |
| B7 recover auto-rejoin | depends on B3 | ✅ `feat/stage-c2-…`/`b7-recover-rejoin` |
| C2 Projects frame | depends on B2 | ✅ `feat/stage-c2-projects-frame(-v2)` |

**At the time, the roof (B5/B6/B7/C2) had been built on a foundation that was
never poured (B1–B4).** Nothing group-related was then on `main`, and the
downstream branches could not function against that snapshot. The replacement
Project Groups architecture subsequently shipped and made this diagnosis
historical.

**Superseded resume guidance (completed via the replacement architecture):**
1. **Salvage-audit the four orphan branches** — keep any self-contained code (e.g.
   the project-summary *resolution logic* in B5, the Projects-frame *TUI* in C2 may
   be partly independent), discard the rest. Don't try to merge them as-is.
2. **Re-run the pipeline in dependency order from B1** — groupstore → project-key →
   join/leave → introductions — as an autopilot/pipeline run (the impl spec is
   already decomposed into bounded-context job briefs; it's ready to execute).
3. Then layer B5–B8 and the TUI (C2/C5) back on top of a real foundation.

> **This is the single most valuable clean-up in the whole list** — it converts
> "confusing half-baked" into "specced and ready to execute in order."

---

### 3.6 Feature #8 — open project (local, or clone from GitHub/GitLab)

**What you asked for:** warden should offer to **open a git project locally** or
**clone it from GitHub/GitLab**, then be ready to work on it.

**Current state — ✅ done on `main`.** Persisted projects, local/remote/new project
operations, tabs, tree nesting/hibernation, and Open Project menus shipped across
the five phases in [`2026-08-28-project-centric-ui.md`](2026-08-28-project-centric-ui.md).

**Historical pre-implementation state:** It was the collaboration-groups design
§6 (the `o` = **Open Project** panel: recent / open-local / open-via-git-clone into
`~/.warden/workspace/<project>`, which auto-spawns the project orchestrator) and impl
job **C5** (`internal/tui/open_project.go`, `internal/projectstore`). **No branch
exists for C5**, and it depends on B2 (project-key) + B3 (join/leave) — both part of
the un-poured foundation above.

**Superseded resume guidance:** this was sequenced after the project foundation and
has now shipped.
The clone mechanics themselves are easy (`git clone` / `go-git PlainClone` already
appear in rotate/backend code) — the real content is the project-key normalization
(B2) and the auto-orchestrator spawn (B3), which is why it's gated on the group work.

**Open question:** auth for private clones (GitHub/GitLab tokens, SSH keys) — reuse
the host's existing git credentials (simplest), or manage per-provider tokens in
warden? Recommend: **reuse host git credentials** for v1, no new secret store.

---

## 4. Layer 2 — the hub (feature #6)

### 4.1 Feature #6 — remote access via **warden-hub** (same user)

**What you asked for:** reach your own daemon over the internet, from any device,
without a VPN.

**Current state — 🟠 wire protocol merged, everything else planned.** This is a
big, well-designed, barely-started feature.
- **Spec:** `docs/specs/2026-08-23-warden-hub.md` — thorough. Daemon dials *out* to a
  hosted hub (NAT-friendly, WSS/443), yamux mux → the **same `s.router()`** serves
  over the relayed listener (**zero route changes** — the key insight). Native
  clients get **true end-to-end mTLS** (per-user CA, hub relays only ciphertext);
  web is hub-terminated TLS.
- **Merged (PR #299):** `relay/wire/` — the dependency-free on-the-wire contract
  (enrollment CSR flow, ECDSA challenge-response auth, per-client stream kinds
  `NativeE2E`/`WebTerminated`/`Control`, `Scope` enum mapping 1:1 onto the daemon's
  `authScope`). **Imported nowhere yet.**
- **NOT built:** the daemon-side `internal/relay` connector, the `authorize()`
  relay-identity branch (the enum + single decision point are already shaped for
  it), the `warden hub register|login|connect` CLI, the config block, and the
  **entire hub server** (accounts, daemon registry, per-user CA, rendezvous/relay,
  dashboard).

**Resume here:** two parallel tracks —
1. **Daemon connector** (`internal/relay`) — dial WSS, enroll/auth with the merged
   `relay/wire` contract, maintain the mux + heartbeat + reconnect, and serve the
   existing router over relayed streams. Add the one relay-identity branch to
   `authorize()`.
2. **Hub server** — a *separate private repo* per the product plan (Go, chi,
   coder/websocket, hashicorp/yamux, **Postgres**, **step-ca** for the CA, an auth
   provider supporting GitHub+GitLab+Google, HTMX+templ dashboard, Fly.io).

**Product/strategy decisions already recorded in the spec (confirm they still
hold):**
- **Option C:** daemon + clients stay **open-source under `srjn45`**; **warden-hub is
  private, hosted-only, not self-hostable**.
- Move warden-hub to the `spinformati` org **only at commercialization**, and only
  after clearing the **employment-IP caveat**. Keep it personal until then.
- Every daemon change is **opt-in + zero-config**: the connector activates *only*
  when `hub.url` + credentials are set. A default install stays a loopback/LAN local
  agent-manager.

**This is the gate for the whole top of the stack.** #7 and #9 are impossible
without it, so if multi-machine/multi-dev matters, **#6 is the priority after the
Layer-1 clean-up.**

---

## 5. Layer 3 — multi-machine / multi-developer (require the hub)

### 5.1 Feature #7 — warden cluster (multiple wardens, one task, across machines)

**What you asked for:** multiple warden daemons on different machines cooperating to
complete a task, using warden-hub as the transport.

**Current state — 🔴 not started.** Parked as roadmap #14 ("distributed warden") and
explicitly deferred in collaboration-groups §8 ("needs the hub"). No code, no
dedicated spec.

**Design direction (sketch — to be specced after #6):**
- The hub already gives a daemon a stable address (`hub://<daemon-id>`) and identity.
  A cluster is **collaboration groups (#5) with a remote transport** — the spec even
  notes the roster descriptors are kept transport-agnostic (`group/<name>/<project-key>`)
  precisely so "the hub version is the same bus, remote transport."
- So the natural shape: **an orchestrator on machine A delegates a job to an
  orchestrator on machine B** via the same `send_message` bus, now relayed through
  the hub. B spawns its own worker in its own worktree and opens a PR in its repo
  (git-anchored — nobody edits a repo they don't own).
- Open hard problems to spec: cross-machine work *attribution* and result
  aggregation, shared-artifact handoff (git is the substrate — push/PR, not file
  copy), and failure/partition handling.

**Dependency:** #6 (hub) **and** #5 (groups) — cluster is the remote generalization
of both. Do not start before both land.

---

### 5.2 Feature #9 — warden-teams (multiple developers share project knowledge)

**What you asked for:** multiple developers working on the same project share common
knowledge/context, using warden-hub.

**Current state — 🔴 not started, no spec.** But the building block is shipped:
backend-neutral **project memory** (`.warden/memory.md`, feature #53, curated from
fleet digests, projected into every backend). Today that memory is **local + committed
per-repo**. "Teams" is the multi-developer generalization of it.

**Decided (2026-08-28): teams is HUB-HOSTED, not git-native.** This is deliberately
a *bigger* feature than "share the committed `.warden/memory.md`." It will include
sharing **live-agent context and knowledge** across a team — e.g. what each
developer's agents are currently doing/learning, not just static committed memory.
The git-native slice is explicitly **not** the MVP.

**Design direction (deep-dive deferred to when #6 lands):**
- Substrate: a **hub-side shared context store keyed by team**, live-synced across
  the team's daemons — built on the hub transport and its **multi-user authz**
  (grants/scopes sketched in hub spec §6.3; roadmap #31 "multi-user" is the
  prerequisite that's still only sketched).
- In scope for the deep-dive (not designed yet): live-agent presence/knowledge
  sharing across developers, shared project knowledge beyond `.warden/memory.md`,
  team-scoped grants and visibility, and how much of #53's curation pipeline
  generalizes to a team store.

**Dependency:** hard on #6 (hub) + multi-user authz. **Needs a dedicated deep-dive
design pass** — this is a feature-rich surface, not a thin generalization of project
memory. Do not scope it until the hub exists.

---

### 5.3 Feature #11 — Backend Hardening (Tier-C → Tier-A)

**What you asked for:** Now that multiple agents are supported, build out the backend-specific adapters so that paid/advanced agents (Cursor, Aider, OpenCode) are as flawlessly integrated as Claude.

**Current state — 🟡 Partial.** The interface (`agentbackend.Backend`) exists, but many adapters mock out the harder semantic methods.

**Design direction:**
- **Transcripts:** Reverse-engineer Cursor's SQLite `store.db` and OpenCode's JSON logs to implement `ParseTranscript`, bringing them into the TUI.
- **Costs:** Wire up native real-time token tracking for all tools so `/usage` isn't blindly guessing.
- **Interactivity:** Ensure `PromptSeeder` and `Reviewer` interfaces are implemented fully where applicable so the orchestration layer can manipulate them programmatically without user intervention.

---

### 5.4 Feature #12 — top-level cross-backend recovery / resume

**What you asked for:** when an agent exhausts its Claude, Antigravity, Codex, or
other provider quota, recover everything useful from that agent and continue the
same task on another backend without losing its branch, worktree, decisions, or
next step. This should be an obvious top-level warden operation, not an expert-only
combination of lifecycle internals.

**Current state — 🟡 the engine and CLI exist; the recovery product is incomplete.**
`internal/lifecycle/switch.go` already implements the central primitive and the CLI
already exposes it as `warden switch`. It retires the active backend process,
extracts a structured handoff, writes it to
`.warden/handoff-<agent-id>.md`, and starts the successor in the **same agent
record, worktree, branch, permission mode, role, and tmux session identity**.
The successor can be pinned explicitly (`--backend` / `--model`) or chosen through
the quota-aware router (`--tier` / `--role`). Automatic soft/hard-limit handover in
#4 calls the same lifecycle machinery.

The manual recovery performed on 2026-08-30 was:

```sh
# Inspect the stopped agent and its last output first.
warden status development-acfe8712 --json
warden tail development-acfe8712 --lines 220

# Atomically replace Antigravity with Codex in the same worktree.
warden switch development-acfe8712 \
  --backend codex \
  --reason quota \
  --prompt 'Verify the recovered worktree and diff, finish validation, self-review, commit, push, and open the requested PR.' \
  --json
```

The switch launched `codex` in `.worktrees/development-acfe8712` with the generated
handoff as its first instruction. The agent ID and branch did not change. Pinning
`--backend codex` was important: a normal `handoff --retire`/`rotate` inherits the
source launch configuration and would therefore have relaunched the exhausted
Antigravity backend. This is the semantic distinction the product should make
clear:

| Operation | Conversation/task context | Worktree | Source agent | Backend |
|---|---|---|---|---|
| `restore` | native backend session | same | revived | unchanged |
| `handoff --retire` / `rotate` | structured task handoff | same | retired | normally inherited |
| `fork` | native conversation fork | new sibling | kept | inherited; Codex-only |
| `switch` / proposed `recover` | structured recovery handoff | same | backend process replaced | explicitly selected or routed |

#### Recovery contract

A first-class recovery operation should be transactional and make these guarantees:

1. **Inspect before mutation.** Resolve the exact agent; capture status, recent
   terminal output, structured transcript/digest, original prompt, role, parent,
   tags, permission mode, branch, worktree, dirty state, and pending approval or
   rate-limit reason.
2. **Preserve ownership.** Never create a second writer for the same worktree. Stop
   or quiesce the source process, but keep the agent record, worktree, branch, dirty
   files, and parent/child topology.
3. **Create a self-contained handoff.** Record Goal, Decisions, Modified Files,
   Git Diff summary (or full bounded patch reference), checks already run and their
   results, failures, immediate next step, original prompt, recent transcript tail,
   and why the switch occurred. Backend-native transcript extraction is preferred;
   deterministic fallbacks must fill gaps.
4. **Select a viable successor.** An explicit backend/model pin wins. Otherwise use
   the unified resolver, excluding the source backend and every backend currently
   disabled, quota-limited, above threshold, or already tried during this recovery.
5. **Spawn before final retirement.** Write the handoff durably, launch the
   successor in the same worktree, verify that its process reaches a live state and
   consumes the continuation prompt, then finalize the old process retirement. A
   failed launch must leave the task recoverable and safe to retry.
6. **Verify postconditions.** Report old/new backend and model, handoff path, agent
   ID, worktree, branch, dirty-file count, successor state, and an audit event. The
   result should clearly say whether the successor is merely spawned or has
   acknowledged the handoff.
7. **Remain idempotent.** Repeating the request after a timeout must detect an
   already-running successor and return its state rather than spawn another writer.

#### Gaps exposed by the real recovery

- **Sparse handoff from Antigravity.** The generated handoff had empty Goal,
  Decisions, Modified Files, Next Step, and Git Diff fields even though `warden
  tail` contained a rich trajectory. The operator had to reconstruct and inject a
  summary manually. `switch` needs a deterministic fallback chain:
  structured adapter transcript → stored original prompt + recent normalized turns
  → terminal tail → git/worktree inspection. Empty fields should be treated as a
  degraded extraction, not a successful complete handoff.
- **Edits were in the wrong worktree.** The source transcript showed absolute paths
  under the shared repository, while its registered worktree was
  `.worktrees/development-acfe8712`; its own branch was clean. Recovery therefore
  had to identify the eight task-scoped diffs in the shared root, exclude unrelated
  user files, check collaboration ownership, and copy only those diffs into the
  correct worktree. Add a pre-switch **worktree consistency audit** comparing
  transcript paths, process cwd, registered worktree, and dirty files. Never move
  shared-root changes automatically unless provenance is unambiguous; present a
  recoverable patch plan or require confirmation.
- **Session-store failure after switching.** `warden status`, `tail`, `send`, and
  `wd check` began failing because an active ScrivaDB segment contained a malformed
  record (`invalid character 'p' looking for beginning of value`). The Codex process
  was nevertheless alive in tmux. Recovery/status must degrade to the process and
  transcript surfaces when one session record cannot decode, quarantine/report the
  corrupt entry, and keep unrelated records readable. A single bad record must not
  block checks, messaging, or inspection for the recovered agent.
- **No acknowledgement phase.** `switch --json` reported the new backend immediately
  while the successor was still starting. Add `--wait` / `--wait-timeout` and a
  structured `acknowledged` state after the successor reads the handoff or emits its
  first turn.
- **Discoverability.** `switch` is top-level but absent from the everyday lifecycle
  quick reference and easy to confuse with `restore`, `rotate`, and `fork`. Surface
  it in `warden --help`, the MCP catalog, skill quick-reference, TUI agent actions,
  web agent menu, and rate-limit notifications.

#### Proposed top-level UX

Keep `warden switch` as the precise low-level verb and add a task-oriented alias:

```sh
# Explicit recovery target.
warden recover-agent development-acfe8712 --backend codex --reason quota --wait

# Let the resolver select the best non-exhausted tier-2 candidate.
warden recover-agent development-acfe8712 --tier tier-2 --reason quota --wait

# Preview evidence, handoff completeness, worktree consistency, and successor.
warden recover-agent development-acfe8712 --backend codex --dry-run
```

MCP/REST should expose one typed operation (for example
`recover_agent_backend`) returning `{agent, from_backend, to_backend, from_model,
to_model, reason, handoff_path, handoff_completeness, worktree_audit,
acknowledged, degraded_reasons}`. The TUI/web action should be **Resume on another
backend…**, preselect the router recommendation, show quota/cooldown state, preview
the recovered context, and require confirmation only for ambiguous shared-worktree
recovery—not for the normal same-worktree hot-swap.

**Acceptance criteria:** a fixture agent for every resumable backend can be forced
into a quota-exhausted state and recovered onto every compatible successor; dirty
tracked and untracked files remain byte-identical; no duplicate writer exists;
the successor receives non-empty goal/next-step context; retry is idempotent;
backend/model selection and exclusion are tested; sparse transcript and corrupt
single-record scenarios degrade visibly but do not strand the task; and an
end-to-end test proves Antigravity → Codex continuation through check, commit, push,
and PR creation.

**Relationship to #4 and #11:** #4 remains the automatic policy/trigger; #12 is
the explicit operator recovery product and its transactional guarantees; #11
improves backend adapters so #12 receives richer transcripts. They should share
one `Lifecycle.HotSwap` implementation and one handoff schema rather than drift.

---

## 6. Cross-cutting — feature #10: generic agent TUI CLI (spaiSH-backed)

**What you asked for:** build a warden CLI that is a **generic agent TUI** (possibly
built on **spaiSH**, your other project), backed by **headless agents** (running in
hidden tmux sessions inside the warden TUI) created via the different AI backends.

**Current state — 🔴 external / not tracked in this repository.** There is no
warden-side implementation or integration spec; add a client seam only when the
external CLI requires one.

**Decided (2026-08-28) — the CLI itself is a separate effort.** spaiSH was built
with a different purpose in mind. Srajan will, **separately from warden**, try to
convert spaiSH into an AI CLI; if that proves impractical, he'll build a fresh
**`spai-cli`** instead. Either way the generic agent CLI/TUI is produced *outside*
this repo — **warden's job is only to be the backend/fleet it drives**, not to build
the TUI. So #10 has no warden-side design work until that CLI exists and needs an
integration seam.

**How it fits what already exists:**
- warden **already** runs every backend as a headless/tmux-hosted agent and has a
  TUI cockpit + PTY attach bridge (`internal/daemon/attach.go`). So the "headless
  agents in hidden tmux, surfaced in a TUI" substrate is *done* — the external CLI
  is really a **new, generic front-end** over it.
- The integration seam is warden's existing daemon API (REST + SSE + attach WS) —
  spaiSH/`spai-cli` becomes *another client*, like the web cockpit and Android app.
  Recommended boundary when it comes: **thin renderer over the daemon API**, not
  embedded lifecycle logic (mirrors the web/Android clients, avoids divergence).
- Once the **hub (#6)** exists, that CLI can target `hub://<daemon-id>` and become a
  first-class remote TUI — so it compounds with #6.

**Dependency:** none hard on warden. Blocked on the *external* CLI existing first
(spaiSH-converted or new `spai-cli`). Lower priority than #6 (see §7/§8.6). Needs an
integration spec only once that CLI is ready.

---

## 7. Recommended sequence

Ordered by **dependency + confusion-reduction + leverage**:

1. ✅ **Tier trio (#1 + #2 + #3) — shipped.** It introduced the persisted task
   dimension and initial-spawn resolver wiring.
2. ✅ **Project Groups (#5) — shipped.** The replacement architecture completed
   all five phases and removed the obsolete specs.
3. ✅ **Open Project (#8) — shipped.** Local, remote, and new-project flows now
   use persisted projects in the project-centric cockpit.
4. **Build #6 warden-hub** — daemon connector + hub server MVP. **Confirmed higher
   priority than the external CLI (§8.6).** The gate for everything multi-machine;
   do it deliberately, native-mTLS from day one. Private-hosted (§8.3).
5. **Then #7 cluster and #9 teams** — both ride the hub transport. #9 teams is a
   **hub-hosted, feature-rich deep-dive** (live-agent context/knowledge sharing,
   §8.4), not a thin memory generalization — spec it properly after #6.
6. **#4 handover** — ✅ done + polished: the two trigger paths are reconciled
   (hard-limit → hot-swap) and proactive quota tracking feeds `GetHeadroom`. Only
   event-surfacing / handover-budget remain optional.
7. **Feature #11: Backend Hardening (Tier-C → Tier-A)** — Expand the `agentbackend.Backend` adapters to perfectly integrate newer agents (Codex, OpenCode, Cursor, Aider). This includes parsing their native JSON/SQLite transcripts into Warden's neutral `Turn` format, wiring up their specific token/cost tracking, and implementing full `Reviewer` and `PromptSeeder` capabilities where applicable.
8. **Feature #12: cross-backend recovery / resume** — promote `warden switch` into
   a discoverable, transactional recovery workflow with complete context fallback,
   worktree-consistency checks, backend exclusion, acknowledgement, idempotency,
   and degraded session-store handling (§5.4).
9. **External CLI (spaiSH / `spai-cli`, #10)** — proceeds **outside this repo** on
   Srajan's separate track (§8.5); warden does nothing until that CLI needs an
   integration seam, and it compounds best once #6 exists.

---

## 8. Decisions — RESOLVED 2026-08-28

1. **Behavior/task rename (#1): KEEP `role`, split out `task`.** No rename of the
   `role` package/CLI/MCP/API surface. `role` stays the persona+defaults concept;
   `task` is introduced as the separate first-class dimension and becomes the tier
   source. Purely additive.
2. **Group-branch salvage (#5): DISCARD the orphan branches.** Drop
   `feat/stage-b5/b6/b7/c2`, salvaging only self-contained fragments; rebuild from
   B1 in dependency order rather than merging roof-onto-missing-foundation.
3. **Hub product posture (#6): private-hosted, unchanged.** warden-hub stays
   private/hosted-only (Option C); daemon+clients stay OSS. (Employment-IP caveat
   still gates any move to `spinformati` at commercialization.)
4. **Teams substrate (#9): HUB-HOSTED, feature-rich — NOT git-native.** Teams is a
   larger feature including **live-agent context/knowledge sharing** across a team,
   built on the hub + multi-user authz. Deliberately deferred to a **dedicated
   deep-dive** when #6 is in progress. The git-native `.warden/memory.md` slice is
   explicitly not the MVP.
5. **External CLI (#10): built OUTSIDE warden.** Srajan will separately convert
   spaiSH into an AI CLI, or build a new `spai-cli` if conversion isn't feasible.
   warden's role is only to be the backend/fleet that CLI drives (thin-client seam
   over the daemon API). No warden-side work until that CLI needs the seam.
6. **Priority: #6 hub is HIGHER priority than the external CLI (#10).** After the
   Layer-1 clean-up, the hub is the next major push; #10 proceeds on its own
   external track and compounds with the hub later.

---

## 9. Pointers

- Specs referenced: `agent-roles.md`, `tiered-model-routing.plan.md`,
  `2026-08-06-backend-registry.md`, `2026-08-23-warden-hub.md`,
  `2026-08-26-collaboration-groups.md`, `2026-08-26-collaboration-groups-impl.md`
  (all under `docs/specs/`).
- Key code seams named above: `internal/role`, `internal/task`,
  `internal/backendstore`, `internal/router`, `internal/lifecycle/switch.go`,
  `internal/poller`, `internal/daemon/middleware.go` (`authorize()`),
  `internal/daemon/attach.go`, `relay/wire/`, `internal/ctxstore`,
  `internal/mailbox`, `internal/pipeline`.
- Shipped foundations this builds on: FEATURES.md (§ backends, § project memory,
  § collaboration MVP).

---

## 7. Cross-cutting — feature #11: Backend-Specific Deep QA & Paid Tier Verification

**What you asked for:** Because Warden was initially developed purely with Claude, many implicit assumptions (like token compaction syntax or usage metric formatting) were baked in. Recently we fixed these "smaller things" for the paid version of Antigravity. In the future, we need to do this 1-by-1 for every supported backend, specifically verifying against their paid/premium versions to ensure Warden works flawlessly across the board.

**Current state — 🟡 ongoing discovery.** We have abstracted the parsers for `ctxtokens` and `spend` dynamically based on `Backend` (shipped for Antigravity). 

**Design direction / Next steps:**
- **Audit 1-by-1:** Systematically audit each backend implementation (OpenAI, Gemini, etc.) using their paid/premium tiers.
- **Backend interface expansion:** Continue isolating backend-specific quirks behind interfaces (like `TokenParser` and `SpendParser`).
- **Paid Feature Parity:** Verify long-context capabilities, exact token tracking, tool calling semantics, and rate-limit recovery for each premium backend.
