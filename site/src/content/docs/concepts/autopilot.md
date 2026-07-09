---
title: Autopilot — concepts
description: The core model behind warden's autonomous run mode — plan, run, brain, ledger, guardian, and cost-tier backend selection.
---

> ⚠️ **Autonomous operation is inherently risky.** When autopilot is enabled, a
> "brain" agent orchestrates worker agents **without human intervention**. Review
> the [Autopilot guide](../guides/autopilot) — in particular the risk warning and
> kill switch — before enabling.

Autopilot is warden's **long-running autonomous mode**: you author a goal in a
plan file, enable autopilot, and warden spins up a *brain* agent that orchestrates
worker agents, lands their branches into an integration branch, heals itself when
it gets stuck, and escalates to progressively cheaper backends if rate-limited —
all without waiting on a human.

---

## Core concepts

### Plan

A YAML brief you author in your repository (`autopilot.plan.yaml` by default).

```yaml
version: 1
goal: "Ship the notifications feature"
constraints:
  - "all changes behind a feature flag"
tasks:                        # optional — the brain authors these if absent
  - id: api
    prompt: "Implement the notifications REST API per docs/specs/notify.md"
  - id: ui
    prompt: "Implement the notifications UI components"
    after: [api]
```

| Field | Required | Notes |
|---|---|---|
| `version` | yes | Must be `1` |
| `goal` | yes | What the run is trying to accomplish |
| `constraints` | no | Injected verbatim into every brain and worker spawn |
| `tasks[].id` | no | Unique within the plan; brain authors tasks if omitted |
| `tasks[].prompt` | no | Worker's task description |
| `tasks[].after` | no | Dependency edge ids |

The plan file is **owner-editable mid-flight** — the brain re-reads it on each
planning cycle. Changing constraints takes effect immediately; adding tasks is
picked up on the next planning tick.

### Run

The daemon's execution of one plan: one brain + its workers + its ledger. The run
id is a stable hash of the repo path and the plan file path, so enabling autopilot
on the same repo + plan always continues the same logical run (the ledger
persists landed tasks across brain restarts).

**At most one active run per repository.** Enabling a second plan on the same repo
fails with a conflict error.

### Brain

A single long-lived headless agent (role `autopilot`) that the Controller spawns
on the **cheapest available backend** (see Cost-tier backend selection below). The
brain:

- Reads the plan file and decomposes the goal into tasks (if absent from the plan)
- Spawns worker agents via warden's MCP tools, tagging each with `autopilot` and `run:<run_id>`
- Watches for landed tasks and re-plans when the task set changes
- Routes approval prompts to itself (not to the operator) while autopilot is active
- Heartbeats so the guardian can detect if it stalls

The brain is a regular warden agent — you can tail its output, inspect it with
`warden status`, and the guardian heals it when it stalls.

### Workers

Agents (and pipelines) the brain spawns to do the actual coding work. Every
worker is tagged `autopilot` + `run:<run_id>`. Workers operate exactly like
normal warden agents: they have their own isolated worktrees, run the project's
checks, commit via `warden commit`, push, and open a PR. When a worker finishes,
the brain calls `warden land` to merge its branch into the integration branch.

**Manual agents are invisible to autopilot's destructive paths** — autopilot
never terminates or modifies an agent that doesn't carry the `run:<run_id>` tag.

### Ledger

The daemon's durable record of run state: which tasks are pending / in-progress /
landed, which branch each landed task produced, and how many heal attempts have
been made. Written authoritatively by the daemon (landings) and by the brain
(task state transitions). Persisted across brain restarts and daemon restarts —
re-enabling autopilot after a disable continues from where the ledger left off.

Ledger task state machine:

```
pending → assigned → in_progress → pr_open → gated
  gated → landed          (gate passed; authoritative, daemon-written)
  gated → fixing → gated  (gate red / conflict; brain heals or respawns worker)
any → replanned            (brain revises decomposition; audit-logged)
```

### Integration branch

`autopilot/integration` (configurable via `autopilot.integration_branch`). The
**only** branch autopilot merges worker branches into. It is never merged into
`main` automatically — the operator reviews it and fast-forwards it to `main`
when satisfied.

**Boundary invariant:** workers never commit to `main` directly. The integration
branch is a staging area for owner review.

---

## Guardian

The daemon's heal loop that keeps the brain alive. It runs continuously while
autopilot is active and fires when the brain's heartbeat goes stale (wedged with
pending work). The heal ladder:

| Stage | What the guardian does |
|---|---|
| 1 — nudge | Send a steering message to the brain |
| 2 — restart | Terminate and restart the brain on the same backend (fresh context, ledger cold-start) |
| 3 — rotate | Restart the brain on the next backend down the cost tier |
| 4 — backoff | All backends exhausted or rate-limited: wait (capped-exponential backoff), notify, then retry from stage 1 |

The guardian **never parks permanently** — it always eventually retries. Backoff
state is visible in `warden autopilot status` (fields: `backoff`, `tier`,
`last_heartbeat`, `context_level`).

A planned rotation (context critical or cadence interval reached) is also handled
by the guardian, cleanly: the brain saves a summary to the ledger, the guardian
restarts it on a fresh context, and work continues.

---

## Cost-tier backend selection

The brain spawns on the **cheapest available backend** for the run. The cost ladder,
from cheapest to most expensive:

| Tier | Backends | Notes |
|---|---|---|
| Free | `antigravity` | Google-hosted free tier; first choice when available |
| Subscription | `claude`, `codex` | Your existing plan; no per-token cost on top |
| Gated pay-per-use | any backend with API billing | Requires explicit opt-in in config |

The Controller also exposes `SelectWorkerBackend(runID)` — the brain can use it
to pick the cheapest available backend for each worker spawn, though the brain
may also select backends itself based on the task.

**Known limitation:** rotate (stage 3 of the guardian heal ladder) requires more
than one free-tier backend to exercise meaningfully. If only one backend is
available in the free tier, the guardian falls back directly to backoff after
restart fails.

---

## Ownership guard

Every autopilot-created agent carries an `autopilot` tag and a `run:<run_id>` tag.
The daemon enforces **ownership**: operations that would destructively modify an
autopilot-owned agent (terminate, remove-worktree, hard-delete) from a *different*
agent (one without the matching `run:<run_id>` tag) are rejected with a
`403 not_owned` error. This prevents a manual agent or operator mistake from
silently clobbering an in-flight autopilot worker.

The **operator** (human, CLI, or MCP acting on behalf of a human) can always
terminate or modify autopilot agents directly.

---

## Approval routing

While autopilot is active, the daemon routes approval prompts from worker agents
to the **brain's inbox** rather than the operator's attention queue. The brain
uses its auto-approve policy to answer routine tool-permission prompts without
stalling workers on human input.

The operator's approval queue is unaffected by autopilot — prompts from manual
agents still surface normally.

---

## Run state machine

```
disabled ──enable──▶ starting ──brain healthy──▶ active
   ▲                    │                          │
   │                    │ spawn fails              │ brain wedged / rate-limited
disable (kill switch)   ▼                          ▼
   └───────────────  degraded ◀──────────────── healing
                        │  guardian backoff loop (never parks)
                        └──heal succeeds──▶ active
active ──all tasks landed──▶ complete
  (brain torn down, ledger retained, integration branch ready for owner review)
```

`disable` is the **kill switch** — it is effective at any state. It stops all
new spawns and landings immediately; in-flight workers keep running to completion.
The brain is terminated gracefully. The ledger is retained so a future `enable`
can continue from where the run left off.
