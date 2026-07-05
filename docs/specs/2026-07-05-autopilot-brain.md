# Autopilot Mode — "warden with a Brain" — Master Plan

**Date:** 2026-07-05
**Status:** Approved design (pre-implementation)
**Owner:** Srajan Pathak

> Master plan agreed in design session; owner decisions are locked below.
> Nothing implemented yet — P0 formalization of this document into a full spec
> (state machine, endpoint contract, `land` semantics) is the first stage.
> It maps what we **reuse**, **extend**, and build **new**.

## Context — the problem

warden's north star (per the owner): *plan once for 1–2 weeks, then have warden
build the product step by step, unattended — steering around every prompt and
resuming through every rate limit, minutes to weeks apart.*

Two owner clarifications define the shape:

1. **"We already have the capability; there is no master-plan executor. We need an
   agent that can smartly steer around all scenarios."** → The gap is not more
   deterministic Go loops. It is a **brain**: a long-lived orchestrator *agent*
   that reasons and acts, using warden's tools, to run the whole show.
2. **"Local LLM was a failure — we can't run big models locally. Use the available
   paid & free backends (Claude, antigravity, codex, …)."** → The brain runs on a
   **real, capable backend**, and rotates across paid+free backends when one is
   rate-limited. The local completer is **not** the reasoning engine.

The obstacle-survival mechanics already ship and are reused wholesale:

| Capability | Lives in | Today |
|---|---|---|
| Auto-approve yes/no + rule-matched prompts | `internal/approval` | ✅ |
| Circuit breaker (identical-prompt loop) | `internal/approval/breaker.go` | ✅ escalates to human |
| Rate-limit auto-resume (mins → weeks) | `internal/daemon/ratelimit.go`, `rate_limit.auto_resume` | ✅ |
| Auto-restart errored agents (capped) | `internal/daemon/autorestart.go` | ✅ |
| Stuck/loop/rate-limit detection & classify | `internal/poller` (`looksLikeLoop`, `detectRateLimit`) | ✅ |
| Auto-compaction on context pressure | `internal/poller/context.go` | ✅ |
| Open PR / commit / branch+CI status / prune | `lifecycle/pr.go`, `internal/branchtrack`, `lifecycle/prune.go` | ✅ |
| DAG pipelines, roles, snapshot, coordination, schedule | `internal/pipeline`, `role`, `snapshot`, `collab`, `schedule` | ✅ |
| **Full agent toolset over MCP** (spawn/monitor/commit/push/PR/CI/snapshot/pipeline/message/prune) | `internal/mcp` (60+ tools) | ✅ |
| Multiple real backends | `internal/agentbackend/backends/` (claude, codex, antigravity, cursor, opencode, aider, crush, goose) | ✅ |

**The gap is a brain that drives all of it, and a substrate that keeps that brain
alive.** Everything the brain needs to *act* already exists as MCP tools.

## Decisions locked (owner)

1. **Task source** = **master-plan file + planner** — but the "planner/executor" is
   the **brain agent** reading the plan file as its brief, not a Go compiler.
2. **Merge target** = **a dedicated `autopilot/integration` branch**, CI-gated; the
   brain merges green+mergeable workers there, and the **owner fast-forwards
   integration → main**. Main never moves unattended.
3. **Reasoning engine** = **a real backend agent (paid or free), NOT local LLM**;
   rotate across a **cost-tier ladder** when limited.
4. **Last resort** = **never park** — the brain retries/reasons forever; the
   substrate keeps it alive with capped backoff and notifies, never gives up.
5. **Form** = **headless**, surfaced via a web + TUI status panel (no live pane).
6. **Scope** = **one brain per master-plan (per repo/initiative)**, each on its own
   cost-tier backend ladder.

---

## Architecture — a supervision hierarchy

```
  [ Deterministic substrate — Go, mostly EXISTS ]   ← keeps the BRAIN alive
        auto-approve · auto-resume · auto-restart · heartbeat guardian · backend failover
                              │  keeps alive / restarts / rotates
                              ▼
  [ THE BRAIN(S) — one orchestrator agent per master-plan/repo ]   ← the smart steerer (NEW concept)
        reads master-plan · spawns workers · monitors · commits/push/PR ·
        merges green workers into autopilot/integration · resolves failures by reasoning ·
        cleans up worktrees/branches · respawns next task · forever (headless)
                              │  spawns / steers / heals (via existing MCP tools)
                              ▼
  [ WORKER AGENTS — pipelines / single agents, any backend ]   ← do the actual work
```

The insight: **the brain is "just an agent"** with (a) a strong autonomy persona,
(b) the full warden MCP toolset it already has, and (c) the master-plan as its
brief. warden's remaining job is to **keep the brain alive and unstuck** — that is
the only genuinely new plumbing, and most of it already exists (auto-approve /
auto-resume / auto-restart) and just needs to target the brain and never dead-end
into "ask a human."

### 1. The Brain (NEW first-class concept; mechanically = a persona + a lifecycle)

- A **new built-in role** (e.g. `autopilot`, extending `internal/role` alongside
  `orchestrator`): persona is the full autonomy playbook — *read the plan file,
  decompose it, spawn workers (single or via `create_pipeline`), monitor with
  `list_agents`/`get_agent_output`, steer stuck workers by reading transcripts and
  sending messages/rollbacks, run `check`, `commit`/`push`, open PRs, poll
  `get_branch_status`, **merge green+mergeable workers into `autopilot/integration`
  only** (never main), clean up with `remove_worktree`/prune, pull the next task,
  repeat forever, never ask a human — reason and act.* Three hard playbook rules:
  1. **Ledger hygiene** — keep the run ledger's task state + journal current.
     Restart-safety itself is **by construction**, not persona discipline: all
     side-effectful ops are daemon-mediated and recorded (store/audit/landings,
     idempotent `land`), and the daemon injects a **recovery digest** (plan +
     ledger + landings + live agents + recent audit) into every (re)spawned brain.
  2. **Worker PRs base on `autopilot/integration`**, never main — the gate must
     validate against the branch the work will actually land on.
  3. **Refresh integration before landing** — if `autopilot/integration` is behind
     main, merge main into it and wait for the gate before landing more work.
- **Durable run ledger** — the brain externalizes run state (task ledger,
  decisions, in-flight worker map) into a daemon-store-backed ledger (reserved
  ctx blackboard namespace `autopilot/<plan>`, reusing `ctx_set`/`ctx_get`). A
  fresh brain cold-starts purely from the recovery digest — this is what makes
  restart/rotation safe and "never park" meaningful (not an amnesiac
  resurrection).
- **One long-lived brain per master-plan** is spawned when autopilot toggles ON,
  on the cheapest available backend (§4), with its plan file injected as its
  brief and full MCP access. **Headless** — surfaced only via the status panel.
  **At most one active brain per repo** (Controller-validated): two plans landing
  on the same integration branch would race.
- The brain does planning/merging/cleanup **by calling the MCP tools that already
  exist** — so there is little-to-no new "executor" code. What may be missing is a
  **guarded merge-into-integration tool** (see New).

### 2. Substrate that keeps the brain alive (reuse + retarget)

- **Auto-approve** the brain's own prompts (persona minimizes these) — reuse
  `internal/approval`; **but reroute the breaker away from human-escalation** for
  the brain: a wedged brain is restarted/rotated, not parked.
- **Auto-resume** the brain on rate limit — reuse `internal/daemon/ratelimit.go`.
- **Auto-restart** the brain on crash/error — reuse `internal/daemon/autorestart.go`.
- **Worker approvals route to the brain, never a human** — a worker prompt the
  auto-approve policy can't answer (unrecognized, breaker-tripped) is forwarded
  to the brain's mailbox; the brain answers it (multi-choice defaults to the
  agent's own recommendation). The human approvals inbox only mirrors these
  events for visibility — nothing blocks on it while a run is active.

### 3. Heartbeat guardian (NEW, minimal Go — the one deterministic guardian)

A brain cannot watch itself. A tiny daemon loop (same pattern as
`worktree_prune.go` / `scheduler.go`, launched from `server.go`) checks **each
brain** is *progressing*. The primary heartbeat is **MCP tool-call recency from
the audit log** — a headless brain that has made no tool call in N minutes while
its run has pending work is wedged; this is far less false-positive-prone than
pane parsing. Pane detectors (`looksLikeLoop`, needs-input) remain as secondary
signals. If a brain wedges: restart it (fresh context, cold-start from the run
ledger), or **rotate its backend down the cost ladder** (§4). Capped exponential
backoff, notify each escalation, **never park** (owner choice #4).

The guardian also performs **planned rotation**: at a context-pressure threshold
(or a configurable cadence) it proactively rotates the brain — the run ledger
makes this a cheap, lossless handoff instead of degrading via repeated
auto-compaction over a weeks-long run.

### 4. Cost-tiered backend failover (NEW — "use any & all backends, cheapest first")

The owner classifies backends into **three cost buckets** and warden always picks
the cheapest available:

- **Free-tier** backends — used **first** for all thinking/steering.
- **Fixed-subscription** (flat-rate) backends — used **only once every free tier is
  exhausted** (rate-limited past reset).
- **Pay-per-use** backends — used **only if the owner has explicitly granted
  permission** (`allow_pay_per_use: true`); otherwise never touched, even if it
  means the brain waits for a free/subscription tier to reset.

Selection is a **descending-cost ladder**: try each backend in the current tier;
when all are limited, drop to the next tier; pay-per-use is gated behind explicit
permission. When the active backend hits its limit, warden **rotates the brain to
the next available backend** so orchestration never fully halts (subject to the
gate). Reuse the backend registry + `handoff_agent` / `rotate_agent`; the
master-plan + worker fleet persist across rotation (they are external to the brain).
The same ladder governs which backend the brain gives new **worker** agents.

### 5. Master-plan file = the brain's brief (not a Go-compiled DAG)

```yaml
# autopilot.plan.yaml
goal: "Ship the notifications feature"
constraints: ["all changes behind a feature flag"]
tasks:                       # optional; the brain will author/refine these
  - id: api
    prompt: "Implement the notifications REST API per docs/specs/notify.md"
  - id: tests
    prompt: "Add integration tests"
    after: [api]
```

The brain reads it, decomposes with judgment, and executes. Owner edits the file
mid-flight to steer; it stays the source of truth.

---

## Reuse vs Extend vs New

### Reuse as-is
The **entire MCP toolset** (spawn/monitor/commit/push/PR/branch-CI/snapshot/
pipeline/message/prune) — the brain's hands. Plus `internal/pipeline`,
`lifecycle.CreatePR`, `internal/branchtrack`, `internal/approval`,
`internal/daemon/ratelimit.go`, `autorestart.go`, `internal/poller` detectors,
`internal/snapshot`, `internal/notify`, `internal/audit`, the daemon
background-loop wiring in `server.go`, the backend registry.

### Extend
- **`internal/role`** — new `autopilot` brain persona (the autonomy playbook).
- **`internal/approval`** — for the brain, reroute breaker escalation from
  "ask human" to the guardian (restart/rotate); optionally answer decision/
  multi-choice prompts with the recommended option so workers don't stall.
- **`internal/config`** — new `autopilot` block; enabling it bundles
  `auto_approve` / `rate_limit.auto_resume` / `auto_restart` on (generous defaults,
  frictionless philosophy).
- **`internal/agentbackend` / handoff** — cost-tiered backend selection +
  brain/worker rotation. Adds a **cost-tier classification** (free / subscription /
  pay-per-use) as owner config — orthogonal to the existing maturity tiers.

### New (small surface)
- **`internal/autopilot`** package — `Controller` (master switch + bundle +
  per-plan brain lifecycle + one-brain-per-repo validation), heartbeat
  **guardian**, cost-tier failover policy. No DAG executor (the brain plans) —
  this is orchestration-of-the-brain, not of the work.
- **Guarded merge-into-integration action** — no MCP merge tool exists today. Add a
  `land`/`merge_agent` MCP tool + `wd land` that merges a worker branch into
  `autopilot/integration` within warden's rail (gate green + mergeable) so
  the brain merges safely instead of raw `gh`. **`land` is idempotent** — landing
  an already-landed branch is a recorded no-op success, so a brain restarted
  mid-action can safely re-issue it; the daemon records landings authoritatively.
  **Gate mode `auto`**: CI when the repo has workflows covering integration PRs,
  else warden's own local `check` rail — a no-CI repo degrades gracefully, it
  never dead-ends the run. The owner fast-forwards integration → main manually
  (or a separate opt-in promotes it).
- **Ownership guard (daemon-side)** — "autopilot touches only autopilot-owned
  agents" is enforced mechanically, not by persona: destructive tools
  (`terminate_agent`, `delete_agent`, `remove_worktree`, …) invoked by a brain
  session are rejected when the target lacks the `autopilot` tag.
- **Toggle surface** (spec-first per the daemon-api-spec-first invariant):
  - `openapi.yaml`: `POST /autopilot {enabled}` + `GET /autopilot` (status: per-plan
    brains — plan, backend, in-flight workers, last heartbeat) → `make generate`.
  - CLI `warden autopilot on|off|status|init` → `make gendocs`. MCP `set_autopilot` /
    `autopilot_status`. TUI keybind + status (`internal/tui`, help in `view.go`).
    Web toggle + status panel (`web/src`).
- **Enable-time preflight + `warden autopilot init`** — ALL friction is
  front-loaded to the one moment the human is present: `init` scaffolds (plan
  template, config block with detected backends, integration branch, CI hint);
  enable preflights plan validity, backend auth/trust per ladder entry, `gh`
  auth, branch existence, gate-mode resolution — and fails with the full
  actionable list. After enable, no path waits on a human.

## Config schema (new `autopilot` block)

```yaml
autopilot:
  enabled: false
  plans:                               # one brain per plan, each on its own ladder;
    - file: autopilot.plan.yaml        # at most ONE active plan per repo
    # - file: other-initiative.plan.yaml
      # budget:                        # optional per-plan cost shaping (soft caps,
      #   max_spawns_per_day: 0        # 0 = unlimited; frictionless defaults)
  brain:                               # default template applied to each plan's brain
    # cost-tiered backend ladder: free first, then subscription,
    # pay-per-use ONLY when allow_pay_per_use is true (owner permission).
    backends:
      free:         [antigravity]      # try first for all thinking/steering
      subscription: [claude]           # only when every free tier is limited
      pay_per_use:  [codex-api]        # only if allow_pay_per_use: true
    allow_pay_per_use: false           # explicit permission gate for paid calls
    role: autopilot                    # the brain persona
    headless: true                     # always headless — surfaced via status panel
    max_parallel_workers: 3
  merge:
    target_branch: autopilot/integration  # brain merges green workers here; owner ff → main
    strategy: squash
    gate: auto                         # auto | ci | local — never merge red (owner
                                       # choice #2); auto = CI when present, else
                                       # warden's local `check` rail
    delete_branch: true                # delete worker branch after it lands
  guardian:
    interval: 60s
    heartbeat_timeout: 10m             # no brain MCP call for this long w/ pending work ⇒ wedged
    rotate_at_context: critical        # planned rotation threshold (ctxtokens level)
    backoff_min: 30s
    backoff_max: 6h                    # cap; never park (owner choice #4)
    notify_each_escalation: true       # via internal/notify (the pinned channel);
                                       # also notifies on landings + run complete
```

## Safety rails (fire only at extremes — frictionless)

Meta-rail: **all friction is front-loaded to the enable-time preflight**; at
runtime no path waits on a human. The docs carry a prominent warning that
leaving autopilot on is inherently risky — the mitigations are the kill switch,
the integration-branch boundary, and the audit log, never runtime prompts.

- Merge target = `autopilot/integration`, never main directly; gate = CI-green +
  mergeable, never merge red (enforced by the `land` tool, not just persona). Main
  moves only when the owner fast-forwards integration. `land` is idempotent —
  restart-safe by construction.
- Autopilot touches only `autopilot`-owned agents (brain + its workers) —
  **enforced daemon-side** (destructive tools from a brain session are rejected
  for non-autopilot targets), not just by persona. Manual agents untouched.
  Toggle-off = instant kill switch for spawning/merging; in-flight agents keep
  running.
- At most one active brain per repo (Controller-validated) — no integration-branch
  races between plans.
- Guardian backoff-capped → no tight spend loop; notifies each escalation.
- **Pay-per-use is opt-in only** (`allow_pay_per_use`): without permission warden
  will wait for a free/subscription tier to reset rather than spend per-call —
  cost can never surprise the owner.
- Respect spend caps / rate-limit machinery (reuse `internal/spend` + `rate_limit`).
- All autonomous merges/cleanups audit-logged (`internal/audit`).
- `wd` rail holds: commits/pushes still pass pre-commit vet + pre-push verify-fast;
  a broken tree blocks and the brain must fix it (no bypass).

## Resolved design gaps (folded in from review)

- **Brain context exhaustion over weeks-long runs** → durable run ledger +
  guardian planned rotation (lossless handoff, not repeated auto-compaction).
- **Idempotency across brain restarts** → write-ahead ledger rule in the persona +
  idempotent `land` with daemon-recorded landings.
- **CI gate validating the wrong base** → worker PRs must base on
  `autopilot/integration`; confirm CI workflow branch filters cover it.
- **Ownership by persona only** → daemon-side ownership guard on destructive tools.
- **Wedge detection via pane heuristics** → primary heartbeat = brain MCP
  tool-call recency from the audit log; pane detectors secondary.
- **Integration-branch races between plans** → at most one active brain per repo.
- **Integration drift vs main** → persona rule: refresh integration from main
  before landing when behind.
- **Notification channel** → pinned to `internal/notify` (guardian escalations,
  landings, run complete, pay-per-use gate hit).

Second review pass (smoothness/frictionless focus):

- **No-CI repos dead-ended at the gate** → gate mode `auto`: CI when present,
  else warden's local `check` rail; "never merge red" holds in both.
- **Unrecognized worker approvals parked in the human inbox** → forwarded to the
  brain's mailbox; inbox mirrors only.
- **Write-ahead intent depended on persona discipline** → recovery digest
  composed by the daemon (store/audit/landings are already authoritative);
  brain journaling is hygiene, not correctness.
- **Runtime failures a human would have to fix at 3am** → enable-time preflight
  + `warden autopilot init` (front-load ALL friction to enable).
- **Mid-run plan-file typo could wedge the run** → keep last-good plan + notify.
- **Worker parked hours on a far-off rate-limit reset** → brain may respawn the
  task on the next ladder backend.

## Open risks to flag before building

- **Brain reliability** — a capable backend is essential; failover order matters so
  a rate-limited lead never fully halts orchestration.
- **Cost** — bounded by the cost-tier ladder (free → subscription → gated
  pay-per-use) + optional per-plan budget. Confirm which of the owner's backends
  fall in each bucket and whether `allow_pay_per_use` should ever be on.
- **CI on integration PRs** — repo workflows may filter to `main` only; gate
  mode `auto` degrades to local checks so the run never dead-ends, but adopters
  should extend filters to `autopilot/integration` to earn the stronger gate
  (preflight prints the hint).

---

## Recommended delivery — as an warden pipeline of short-lived agents

Large, multi-phase; build as staged agents (bounded context). Dogfoods autopilot.

| Stage | Deliverable | Kind |
|---|---|---|
| **P0** | Formalize into `docs/specs/autopilot.md`: hierarchy, brain persona (incl. write-ahead/PR-base/refresh rules), run-ledger schema, endpoint contract, **idempotent `land` contract**, guardian algorithm (heartbeat + planned rotation), ownership guard, cost-tier selection | design |
| **P1** | `autopilot` config block + `Controller` (master switch + bundle) + daemon endpoint (spec-first) + CLI/MCP/TUI/web toggle + `warden autopilot init` scaffolder + enable-time preflight. **Visible but inert** switch. | implement |
| **P2** | Brain persona/role + per-plan brain lifecycle (spawn a headless brain per plan on the cheapest available backend, plan-file brief + full MCP). | implement |
| **P3** | Keep-alive substrate retargeted to each brain (auto-approve/resume/restart, breaker-reroute) + heartbeat **guardian** + cost-tiered failover/rotation. | implement |
| **P4** | Guarded `land` merge-into-integration tool (`wd land` + MCP) with the CI-green gate + worker cleanup. | implement |
| **P5** | Docs/website/skill (incl. a prominent "unattended operation is risky" warning + adoption guide around `init`) + `make gendocs` + tag & release (CLAUDE.md DoD). | docs/release |

P2–P4 depend on P1; P5 on all. Build **P1 end-to-end first** (inert switch wired
through every surface) before the brain logic lands.

## Verification approach

- **Unit**: table-driven tests (mirroring `breaker_test.go`, `autorestart_test.go`)
  for the guardian progress-detection, backoff/rotation policy, `land` gate +
  idempotency (re-land = no-op success), ownership guard (destructive tool vs
  non-autopilot target rejected), and ledger cold-start (fresh brain reconstructs
  run state from plan file + ledger + agent list).
- **Integration ($0-ish rig)**: isolated daemon on an alt port (never systemd); a
  **free/cheap backend as the brain** (the local-LLM path is out, so use a real
  free backend for the rig); throwaway git repo seeded via `git commit-tree`; drive
  a 2–3 task plan end-to-end and assert brain → spawn workers → PR → merge → cleanup
  → next.
- **Guardian live**: wedge the brain (loop / rate-limit / crash) and assert it is
  restarted/rotated and resumes, via `autopilot_status` + audit log.
- **Failover live**: exhaust the free tier; assert the brain drops to the
  subscription tier, and that pay-per-use is skipped unless `allow_pay_per_use` —
  plan + workers intact across rotation.
- **Toggle**: flip on/off from CLI/MCP/TUI/web; confirm brain spawn/pause + bundle
  knobs track the flag live (no restart).

## Definition of Done (CLAUDE.md checklist — final stage)

- Tag & release (one tag/feature; confirm before pushing `v*`).
- Docs: README, `docs/FEATURES.md` + root matrix, `docs/USAGE.md`,
  `docs/specs/autopilot.md`, site guides + generated `reference/cli.md`, skill.
- CLI help via cobra + `make gendocs` (CI-gated).
