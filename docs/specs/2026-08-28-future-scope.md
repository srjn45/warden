# Warden Future Scope & In-Flight Audit

**Date:** 2026-08-28
**Status:** Planning / brainstorm (planner mode — no code changes)
**Author:** Srajan (with Claude planner)

This document does two things:

1. **Audits the current state** of ten proposed features — because several were
   started and left mid-way (session limits), and it was unclear which are
   half-baked. For each, it records the exact **"resume here"** point.
2. **Lays out the future scope** as a layered architecture with a sequenced
   roadmap, dependencies, and the open decisions that still need a call.

> **Scope note.** This is a forward-looking planning doc. Shipped behaviour is
> catalogued in [`FEATURES.md`](../FEATURES.md); the older running to-do list is
> [`FUTURE_ENHANCEMENTS.md`](../FUTURE_ENHANCEMENTS.md) (now partly stale — its
> priority matrix parks distributed/multi-user, which this doc un-parks). When an
> item here ships, move it to FEATURES.md and delete it from the roadmap.

---

## 0. TL;DR — status of the ten features

| # | Feature | Status | One-line "resume here" |
|---|---------|--------|------------------------|
| 1 | Roles → **behaviors & tasks** | 🟡 In progress **on main** (Stages 1–3) | Wire the new `internal/task` registry into tier resolution; retire the role-keyed tier map |
| 2 | **Model tiers** across backends → start agent | 🟢 Built (Stages 1–4) | Run **first-spawn** through the resolver (today only hot-swap does) |
| 3 | **Pre-assign tiers** to roles (default + override) | 🟢 Mostly built | Same first-spawn wiring as #2; re-key defaults off *tasks* (see #1) |
| 4 | **Auto-handover** near session limit | ✅ **Done end-to-end** | Nothing blocking — optional polish only |
| 5 | **Collaboration groups** | 🟠 Specced + decomposed, **non-linear half-start** | Abandon the orphan branches; build the **foundation B1→B4 first** |
| 6 | **Remote access via warden-hub** | 🟠 Wire protocol only | Build the daemon `internal/relay` connector + the hub server MVP |
| 7 | **Warden cluster** (multi-machine) | 🔴 Not started | Blocked on #6 |
| 8 | **Open project** (local / clone GH·GL) | 🟠 Specced, not started | Blocked on group foundation (#5 B2/B3) |
| 9 | **Warden-teams** (shared knowledge) | 🔴 Not started, no spec | Blocked on #6; builds on shipped project-memory |
| 10 | **Generic agent TUI CLI** (spaiSH/`spai-cli`) | 🔴 Built **outside warden** | External track; warden only provides the client seam |

**Legend:** ✅ done · 🟢 built, one wire missing · 🟡 in progress on `main` ·
🟠 partial / spec-only · 🔴 not started.

**The headline:** only **#5 and #6** are genuinely "confusing half-baked." #2/#3/#4
are essentially finished, #1 is cleanly mid-refactor on `main`, and #7/#9/#10 are
not-yet-started (so nothing to untangle — just design).

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
     #1 behaviors/tasks   #5 collaboration groups   #8 open project
     #2 tier-at-spawn     #3 role→tier defaults
                       │
  Layer 0  ── foundation (mostly SHIPPED) ──────────────────────────────
     backend registry · tiered model routing · handover/hot-swap · #4
     ctx_* blackboard · directed messaging · pipeline engine

  Cross-cutting:  #10 generic agent TUI CLI (spaiSH) — reskins the client
                  surface across all layers; can start anytime after a design.
```

**Reading of the map:** Layer 0 is done. Layer 1 is where you are actively
working (some done, some tangled). Layer 2 (the hub) is the single biggest lever —
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

**Current state — 🟡 in progress, on `main` (Stages 1–3, PRs #307/#308/#309):**
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

**Resume here (the exact gap):** `internal/task` **is not wired into anything** —
only its own test imports it. Tier selection still keys off the *old* hand-kept
role→tier map in `backendstore/seed.go` (`DefaultRoleTiers`, keyed by
`analysis`/`implementation`/`code-review`…), whose keys are **task names masquerading
as role names**. The refactor's finish line:
1. Make `router.DetermineTargetTier` / `DefaultRoleTiers` consume `internal/task.Tier`
   as the source of a task's default tier, instead of the string map.
2. Give spawn a first-class **task** dimension (today only `Type` carries it) and
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

**Current state — 🟢 built (tiered-model-routing.plan.md Stages 1–4, PRs
#300/#303/#304/#305):** the 3-tier catalog, quota headroom tracking, and the
weighted-headroom `Resolver` (`ResolveOptions{Role, Tier, …} → Resolution`) all
exist and are surfaced via CLI (`warden models …`), MCP (`list_models`,
`set_model_tier`), and REST.

**Resume here (the one missing wire):** the resolver is wired **only into
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

**Current state — 🟢 mostly built.** `RoleTierMapping` + `DefaultRoleTiers` seed
defaults; `DetermineTargetTier` implements the precedence **explicit tier > role/
task default > global default (tier-2)**; `set_role_tier` lets the user override.

**Resume here:** two things, both already named above —
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
- A *separate* older path (`internal/daemon/ratelimit.go`) handles hard rate-limit
  banners (parse reset time → schedule resume → un-pause pane).

**Resume here:** nothing blocking. **Optional polish only:**
- Reconcile the two paths (headroom `DecideHotSwap` vs. banner `RateLimitScheduler`)
  so they don't double-fire.
- Surface handover events more visibly in TUI/web (they happen somewhat silently).
- Consider a "handover budget" so a thrashing backend doesn't ping-pong.

---

### 3.5 Feature #5 — collaboration groups (orchestrators join/leave)

**What you asked for:** the per-project orchestrator from each project can join/leave
named **groups** where orchestrators discover and collaborate with each other.

**Current state — 🟠 fully specced, decomposed, but half-started NON-LINEARLY.
This is the tangle to fix first.**

Specs are thorough and good:
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

**You built the roof (B5/B6/B7/C2) on a foundation that was never poured (B1–B4).**
Nothing group-related is on `main`. The downstream branches almost certainly don't
compile/function against `main` because the store, project-key, and join/leave core
they import don't exist.

**Resume here (recommended):**
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

**Current state — 🟠 specced, not started.** It's the collaboration-groups design
§6 (the `o` = **Open Project** panel: recent / open-local / open-via-git-clone into
`~/.warden/workspace/<project>`, which auto-spawns the project orchestrator) and impl
job **C5** (`internal/tui/open_project.go`, `internal/projectstore`). **No branch
exists for C5**, and it depends on B2 (project-key) + B3 (join/leave) — both part of
the un-poured foundation above.

**Resume here:** this rides on #5's foundation. Sequence it **after B1–B3 land**.
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

## 6. Cross-cutting — feature #10: generic agent TUI CLI (spaiSH-backed)

**What you asked for:** build a warden CLI that is a **generic agent TUI** (possibly
built on **spaiSH**, your other project), backed by **headless agents** (running in
hidden tmux sessions inside the warden TUI) created via the different AI backends.

**Current state — 🔴 not started, no spec.** This is net-new and the most open-ended.

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

1. **Finish the tier trio (#1 + #2 + #3) as one pipeline.** Small, coherent, on
   `main` already. Deliverable: introduce the `task` dimension as the tier source
   (name `role` stays, §8.1); spawn resolves tier via the router. Clears the 🟡
   in-flight state cleanly.
2. **Untangle #5 collaboration groups.** **Discard** the four orphan branches
   (§8.2 — salvage only self-contained fragments), then re-run the impl pipeline
   **from B1 in dependency order**. Highest confusion-reduction value.
3. **Land #8 open-project** on top of #5's foundation (small once B2/B3 exist).
4. **Build #6 warden-hub** — daemon connector + hub server MVP. **Confirmed higher
   priority than the external CLI (§8.6).** The gate for everything multi-machine;
   do it deliberately, native-mTLS from day one. Private-hosted (§8.3).
5. **Then #7 cluster and #9 teams** — both ride the hub transport. #9 teams is a
   **hub-hosted, feature-rich deep-dive** (live-agent context/knowledge sharing,
   §8.4), not a thin memory generalization — spec it properly after #6.
6. **#4 handover** — already done; schedule only optional polish (reconcile the two
   trigger paths, surface events).
7. **External CLI (spaiSH / `spai-cli`, #10)** — proceeds **outside this repo** on
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
