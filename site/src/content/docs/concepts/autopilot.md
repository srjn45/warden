---
title: Autopilot — concepts
description: The core model behind warden's autonomous run mode — plan, run, the manager/worker/resolver topology, overwatch, ledger, guardian, and cost-tier backend selection.
---

> ⚠️ **Autonomous operation is inherently risky.** When autopilot is enabled, a
> **manager** agent drives a fleet of worker agents **without human intervention**.
> Review the [Autopilot guide](../guides/autopilot) — in particular the risk
> warning and kill switch — before enabling.

Autopilot is warden's **long-running autonomous mode**: you author a goal in a
plan file, enable autopilot, and warden spins up a *manager* agent that drives a
fleet of worker agents, lands their branches into an integration branch, heals
itself when it gets stuck, and escalates to progressively cheaper backends if
rate-limited — all without waiting on a human.

## Agent topology

A run is a small fleet with clearly separated jobs:

- **Manager** (role `autopilot`) — one long-lived headless agent per run. It
  decomposes the goal, spawns and steers workers, routes approvals, and lands
  finished work. *(Historically called "the brain"; the ledger key
  `autopilot.brain` keeps that name for back-compat.)*
- **Worker** (role `worker`) — the manager spawns one per task by default. A
  worker owns its task end-to-end — implement → self-review → open a PR on the
  integration branch → drive CI green → merge — and reports status back to the
  manager. For a large task the manager may instead spawn a pipeline of
  `implementer`/`reviewer`/`auto-merger` agents.
- **Resolver** (role `brain`) — an on-demand agent the manager spawns to unblock
  a stuck worker or make an ad-hoc design/architecture decision without human
  interaction, then report the call back. Short-lived; not every run needs one.
- **Overwatch** — a daemon-internal backstop (not an agent) that nudges a
  live-but-quiet manager to tend workers that fall idle or wait on input. See
  [Overwatch](#overwatch) below.

---

## Core concepts

### Plan

A YAML brief you author in your repository (`autopilot.plan.yaml` by default).

```yaml
version: 1
goal: "Ship the notifications feature"
constraints:
  - "all changes behind a feature flag"
tasks:                        # optional — the manager authors these if absent
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
| `constraints` | no | Injected verbatim into every manager and worker spawn |
| `tasks[].id` | no | Unique within the plan; manager authors tasks if omitted |
| `tasks[].prompt` | no | Worker's task description |
| `tasks[].after` | no | Dependency edge ids |

The plan file is **owner-editable mid-flight** — the manager re-reads it on each
planning cycle. Changing constraints takes effect immediately; adding tasks is
picked up on the next planning tick.

### Run

The daemon's execution of one plan: one manager + its workers + its ledger. The
run id is a stable hash of the repo path and the plan file path, so enabling
autopilot on the same repo + plan always continues the same logical run (the
ledger persists landed tasks across manager restarts).

**At most one active run per repository.** Enabling a second plan on the same repo
fails with a conflict error.

### Manager

A single long-lived headless agent (role `autopilot`) that the Controller spawns
on the **cheapest available backend** (see Cost-tier backend selection below). The
manager:

- Reads the plan file and decomposes the goal into tasks (if absent from the plan)
- Spawns worker agents via warden's MCP tools — the daemon tags each spawn with
  `autopilot` and `run:<run_id>` automatically, inherited from the calling
  agent's identity, so the fleet roster never depends on the manager remembering
  to pass tags (this extends to pipelines the manager creates)
- Spawns a **resolver** (role `brain`) on demand when a worker needs an ad-hoc
  design/architecture call it can't make itself
- Watches for landed tasks and re-plans when the task set changes
- Routes approval prompts to itself (not to the operator) while autopilot is active
- Heartbeats so the guardian can detect if it stalls

The manager is a regular warden agent — you can tail its output, inspect it with
`warden status`, and the guardian heals it when it stalls. *(The ledger records
its agent id under the key `autopilot.brain`, which keeps that historical name
for back-compat.)*

### Workers

Agents (and pipelines) the manager spawns to do the actual coding work. By
default a task gets one `worker`-role agent that owns it end-to-end — implement →
self-review → open a PR on the integration branch → drive CI green → merge — and
reports status back to the manager; a large task may instead get a pipeline of
`implementer`/`reviewer`/`auto-merger` agents. Every worker is tagged `autopilot`
+ `run:<run_id>`. Workers operate exactly like normal warden agents: they have
their own isolated worktrees, run the project's checks, commit via
`warden commit`, push, and open a PR. When a worker finishes, its branch is
landed into the integration branch via `warden land`.

**Manual agents are invisible to autopilot's destructive paths** — autopilot
never terminates or modifies an agent that doesn't carry the `run:<run_id>` tag.

### Resolver

A short-lived `brain`-role agent the manager spawns on demand to unblock a stuck
worker — an unanswerable prompt, an ambiguous requirement, or an ad-hoc
design/architecture decision that must be made for the work to continue. The
resolver gathers just enough context, makes a decisive call **without any human
interaction**, reports it back to the manager (and the stuck worker), and
finishes. Not every run needs one.

### Ledger

The daemon's durable record of run state: which tasks are pending / in-progress /
landed, which branch each landed task produced, and how many heal attempts have
been made. Written authoritatively by the daemon (landings) and by the manager
(task state transitions). Persisted across manager restarts and daemon restarts —
re-enabling autopilot after a disable continues from where the ledger left off.

Ledger task state machine:

```
pending → assigned → in_progress → pr_open → gated
  gated → landed          (gate passed; authoritative, daemon-written)
  gated → fixing → gated  (gate red / conflict; manager heals or respawns worker)
any → replanned            (manager revises decomposition; audit-logged)
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

The daemon's heal loop that keeps the manager alive. It runs continuously while
autopilot is active and fires when the manager's heartbeat goes stale (wedged with
pending work). The heal ladder:

| Stage | What the guardian does |
|---|---|
| 1 — nudge | Send a steering message to the manager |
| 2 — restart | Terminate and restart the manager on the same backend (fresh context, ledger cold-start) |
| 3 — rotate | Restart the manager on the next backend down the cost tier |
| 4 — backoff | All backends exhausted or rate-limited: wait (capped-exponential backoff), notify, then retry from stage 1 |

The guardian **never parks permanently** — it always eventually retries. Backoff
state is visible in `warden autopilot status` (fields: `backoff`, `tier`,
`last_heartbeat`, `context_level`).

A planned rotation (context critical or cadence interval reached) is also handled
by the guardian, cleanly: the manager saves a summary to the ledger, the guardian
restarts it on a fresh context, and work continues.

---

## Overwatch

The guardian keeps the manager *alive*; the **overwatch** keeps the manager
*doing its job on the workers*. A manager can be perfectly alive yet quietly stop
tending its fleet, so warden adds a mechanical backstop that never relies on
persona discipline. The overwatch is **daemon-internal — not an agent, not a
scheduled agent** — and derives each run's worker roster purely from the
`run:<run_id>` tag (nothing is persisted; a restart re-adopts the fleet for free).

It nudges a live manager on either of two triggers, **both skipped while the
manager is busy** (a busy manager will see its workers on its own):

| Trigger | When it fires |
|---|---|
| Event-driven | One or more workers fall idle or start waiting on input, debounced to at most one nudge every ~5 minutes |
| Periodic | A heartbeat check-in roughly once an hour, so an idle manager keeps reconciling and pulling the next task |

Each nudge names the needy workers and asks the manager to answer/steer anything
`waiting_for_input` and clean up finished/idle workers before pulling the next
task. The cadences are fixed, generous constants — the overwatch is a **backstop,
not a pacer** (see warden's frictionless-safeguards philosophy). It complements
the guardian, which watches manager liveness; the overwatch **only ever messages
the manager** — it never touches a worker itself.

---

## Cost-tier backend selection

The manager spawns on the **cheapest available backend** for the run. The cost ladder,
from cheapest to most expensive:

| Tier | Backends | Notes |
|---|---|---|
| Free | `antigravity` | Google-hosted free tier; first choice when available |
| Subscription | `claude`, `codex` | Your existing plan; no per-token cost on top |
| Gated pay-per-use | any backend with API billing | Requires explicit opt-in in config |

The Controller also exposes `SelectWorkerBackend(runID)` — the manager can use it
to pick the cheapest available backend for each worker spawn, though the manager
may also select backends itself based on the task.

**Known limitation:** rotate (stage 3 of the guardian heal ladder) requires more
than one free-tier backend to exercise meaningfully. If only one backend is
available in the free tier, the guardian falls back directly to backoff after
restart fails.

---

## Ownership guard

Every autopilot-created agent (manager, workers, resolvers) carries an `autopilot`
tag and a `run:<run_id>` tag. The daemon enforces **ownership**: operations that
would destructively modify an autopilot-owned agent (terminate, remove-worktree,
hard-delete) from a *different* agent (one without the matching `run:<run_id>` tag)
are rejected with a `403 not_owned` error. This prevents a manual agent or operator
mistake from silently clobbering an in-flight autopilot worker.

The **operator** (human, CLI, or MCP acting on behalf of a human) can always
terminate or modify autopilot agents directly.

---

## Approval routing

While autopilot is active, the daemon routes approval prompts from worker agents
to the **manager's inbox** rather than the operator's attention queue. The manager
uses its auto-approve policy to answer routine tool-permission prompts without
stalling workers on human input.

The operator's approval queue is unaffected by autopilot — prompts from manual
agents still surface normally.

---

## Run state machine

```
disabled ──enable──▶ starting ──manager healthy──▶ active
   ▲                    │                          │
   │                    │ spawn fails              │ manager wedged / rate-limited
disable (kill switch)   ▼                          ▼
   └───────────────  degraded ◀──────────────── healing
                        │  guardian backoff loop (never parks)
                        └──heal succeeds──▶ active
active ──all tasks landed──▶ complete
  (manager torn down, ledger retained, integration branch ready for owner review)
```

`disable` is the **kill switch** — it is effective at any state. It stops all
new spawns and landings immediately; in-flight workers keep running to completion.
The manager is terminated gracefully. The ledger is retained so a future `enable`
can continue from where the run left off.

While a run is `active`, two daemon-internal supervisors share the guardian's
ticker: the **guardian** heals manager liveness, and the **overwatch** nudges a
live-but-quiet manager to tend idle/waiting workers.
