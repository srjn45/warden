---
title: Autopilot — autonomous agent runs
description: Adoption walkthrough — create a plan, run it in autopilot mode, watch status, land worker branches, and how to stay safe running agents unattended.
---

import { Aside } from '@astrojs/starlight/components';

<Aside type="caution" title="Unattended operation is inherently risky">
Start a plan with `warden plan run <id> --mode autopilot` — there is no
separate enable step. A **manager** agent then drives a fleet of
worker agents without waiting for human input. Workers write code and open
PRs; the daemon lands them into the integration branch. You should understand
the mitigations before starting:

- **Pause switch:** `warden plan pause <id>` stops new spawns and landings
  immediately (in-flight workers keep running). Use it any time you need to
  regain control.
- **Integration-branch boundary:** workers never merge to `main` directly —
  each run lands into its own integration branch (default `autopilot/<plan-name>`;
  existing runs on `autopilot/integration` are grandfathered). Review before
  fast-forwarding `main`.
- **Audit log:** every autopilot action (manager spawn, worker spawn, land, heal)
  is written to `warden inspect audit` — a permanent, append-only record of what
  ran.
</Aside>

Autopilot lets warden run a **goal-directed, long-lived agent loop** over your
codebase. You describe what you want in a plan
and start with `warden plan run --mode autopilot`. Warden takes care of the rest:
spawning a **manager** agent that breaks the goal into
tasks, delegates each task to worker agents in isolated worktrees, gates their
branches through CI, and lands them into an integration branch — healing itself
when stuck, and escalating to cheaper backends when rate-limited.

## The fleet

A run is a small fleet with separated jobs:

- **Manager** (role `autopilot`) — the long-lived agent that drives the whole run.
- **Worker** (role `worker`) — one per task by default, owning it end-to-end
  (implement → self-review → PR → CI green → merge) and reporting back to the
  manager. A large task may instead get a pipeline of `implementer`/`reviewer`/`auto-merger` agents.
- **Resolver** (role `brain`) — spawned on demand to unblock a stuck worker or
  make an ad-hoc design call, without human interaction.

A daemon-internal **overwatch** backstop keeps the fleet moving: it nudges the
manager to tend workers that fall idle or wait on input. It is **fully automatic —
no user action needed** — and its cadences are generous (a backstop, not a pacer).

For the underlying design — manager/worker/resolver topology, overwatch, ledger,
guardian, cost-tier ladder — see [Autopilot concepts](../concepts/autopilot).

---

## Prerequisites

Before starting autopilot, make sure:

- `warden daemon` is running and healthy (`warden doctor`)
- At least one agent backend is authenticated (`claude --version` for the default
  Claude backend; or configure an alternative in `~/.warden/config.yaml`)
- Your repository has a `main` branch and GitHub Actions CI (or a local CI
  configured in `.warden/check.yml`) — the `gated` step verifies the gate before
  landing
- `gh` is authenticated (`gh auth status`) — autopilot opens PRs and checks CI
  status via the GitHub CLI

---

## Step 1 — create the plan

Plans live in the daemon store, not in a repo file. Create one from inside the
repo you want autopilot to drive (see `warden plan create --help`), and set the
`autopilot` block in `~/.warden/config.yaml` if you want non-default settings:

```yaml
autopilot:
  merge:
    gate: auto            # auto | ci | local (auto picks ci when a workflow covers the branch)
  completion:
    merge_default: true              # bring integration current with the default branch before the final PR
    manager_verify_timeout: 30m      # how long the manager has to verify done_when
    merge_poll_interval: 2m          # how often to poll a green final PR while awaiting owner merge (floor 30s)
```

`warden plan run <plan-id> --mode autopilot` prints a `warnings:` note when no
workflow covers the resolved integration branch — add `autopilot/**` to
`on.pull_request.branches` in one of your `.github/workflows/*.yml` files so
`gate: auto` covers every per-plan branch:

```yaml
on:
  pull_request:
    branches:
      - autopilot/**
```

Unknown subcommands now error: `wd autopilot bogus` exits non-zero with an
unknown-command message (and a suggestion when the name is close). Bare
`wd autopilot` still prints help and exits 0.

---

## Step 2 — add tasks while the plan is pending

Create the plan with `warden plan create` (or `warden plan update` / `warden plan task`
while it is still `pending`). A running plan's definition cannot be edited.
The manager decomposes the goal into tasks if you leave the list empty; or
provide coarse tasks yourself:

```yaml
version: 1
goal: "Ship the notifications feature end-to-end"
constraints:
  - "all changes behind a feature flag named NOTIFICATIONS_ENABLED"
  - "every PR must pass lint and tests"
tasks:
  - id: api
    prompt: "Implement the notifications REST API per docs/specs/notify.md"
  - id: ui
    prompt: "Implement the notification bell and dropdown UI"
    after: [api]
  - id: e2e
    prompt: "Write E2E tests covering the notifications happy path"
    after: [ui]
```

The plan file is **owner-editable while pending** — a running plan's definition
cannot be edited.

---

## Step 3 — configure the cost tier (optional)

By default, the manager picks the **cheapest available backend**. The cost-tier ladder
is now **derived from the [backend registry](/warden/guides/backend-registry/)** — a
backend's tier is whatever you set with `warden backend tier`, and only **installed,
enabled** backends are eligible. So you steer autopilot's spending by
tiering backends:

```sh
warden backend tier antigravity free          # free tier — first choice
warden backend tier claude subscription       # your existing plan
warden backend tier codex subscription
warden backend list                           # verify the ladder
```

| Tier | Typical backends | Notes |
|---|---|---|
| `free` | `antigravity` | Google-hosted free tier; no billing |
| `subscription` | `claude`, `codex` | Your existing plan |
| `pay_per_use` | API-billed backends | Requires explicit opt-in (the paid-autopilot gate) |

To test autopilot at zero additional cost, tier only free backends and leave the
paid-autopilot gate off (the default) so the manager never reaches a `pay_per_use`
backend.

:::note[Deprecation]
The registry **supersedes** the old `autopilot.brain.backends` ladder and
`autopilot.brain.allow_pay_per_use` gate in `~/.warden/config.yaml`. Those keys are
imported into the store **once** on the first boot after upgrade, then ignored (the
daemon warns if they linger). Manage tiers with `warden backend tier` from then on.
:::

---

## Step 4 — create and start the plan

There is **no enable step**: starting a plan is what starts autopilot.

```sh
warden plan create ...                        # canonical plan in the store
warden plan run <plan-id> --mode autopilot    # preflight runs here
# or by name:
warden plan run notifications --mode autopilot
```

Watch and control a running plan:

```sh
warden plan show <plan-id> --watch   # live status: executor state, backoff, integration branch, per-task worker/PR
warden plan pause <plan-id>
warden plan resume <plan-id>
warden plan stop <plan-id>
```

> **Removed:** `warden autopilot enable|on|register|start|disable|off|pause|resume|stop|unregister|list|run`
> no longer exist; typing one prints the `warden plan` replacement. `autopilot init`
> was removed too; use `warden plan create`.

---

## Monitoring a run

```sh
warden autopilot status          # every run's state, manager id, task counts (--json for scripts)
warden plan show <id> --watch    # live status of one plan: executor, backoff, integration branch, per-task worker/PR
warden ls                        # shows the manager + all worker agents
warden status <manager-id>       # full manager detail + events
warden agent tail <manager-id>         # recent manager output
warden inspect audit                 # full append-only audit trail of every action
```

If status shows `preflight_warnings`, the run recovered from a **content-only**
plan issue on daemon restart (e.g. an invalid task status that was normalized to
`pending`). The run is active — edit the plan file to clear the warnings. A run
stuck in `degraded` after a restart on a **legacy file-only** run usually means a
structural problem (missing or unreadable plan file); fix or restore the file and
the watcher will auto-recover without any manual step. **Plan-bound** runs recover
from ScrivaDB, so a missing or bad YAML export never degrades them.

Status shows spawn failures as a backoff object with a `kind` and `last_error`
(`backend_unavailable`, `no_backend_selectable`, `definition_error`,
`spawn_error`). Transient kinds retry with capped-exponential backoff forever.
A `definition_error`, or the same `spawn_error` text repeated
`autopilot.guardian.max_identical_failures` times (default 5, hot-reloadable),
**parks** the run as needs-attention: one notification and one audit event, and
it shows as *waiting* in the tree. To clear it, change the plan in ScrivaDB, run
`wd plan resume` (or `pause` then `resume`), or restart the daemon — each retries
the heal ladder from the top.

**Progress watchdog.** A manager can heartbeat forever while the run goes
nowhere, so the guardian also tracks per-run *progress*: a ledger task state
change, a new landing, a plan task status change, or a spawned worker. Heartbeats
and overwatch nudges do not count. When an active run has made no progress for
`autopilot.guardian.progress_watchdog_window` (default `2h`) **and** no
run-tagged agent is working, the watchdog climbs the same heal ladder
(watchdog-specific nudge naming the stalled tasks → restart → rotate), sharing
the heartbeat grace so the two never double-step. Any progress clears it. If the
ladder is exhausted without progress the run parks as needs-attention
(`no_progress`), notifying once and pointing at `wd plan restart`. The last
progress time is persisted (a daemon restart does not reset the window) and shown
as `last_progress_at` / `watchdog` (`idle|armed|escalating|parked|disabled`) in
run status and `wd plan show`. Switch it off with
`autopilot.guardian.progress_watchdog_enabled: false`; both keys hot-reload.

The TUI cockpit (`warden tui`) shows each run as a **plan-scoped tree** — manager
(`<scope>-autopilot`), guardian (`<scope>-guardian`), plan checklist, and workers
grouped by ledger state. The web dashboard shows an **Autopilot** panel when a run
is active.

---

## Recovering a stuck plan

A run can stall in three ways: you stopped it, the guardian parked it as
needs-attention, or the manager heartbeats but nothing moves (the
progress watchdog parks that as `no_progress`).
`wd plan resume` only undoes a *pause*; for a stopped or parked run use
**`wd plan restart`**.

```sh
wd plan show <plan-id>                    # executor state, last_progress, watchdog, restarts
wd plan restart <plan-id>                 # asks for confirmation
wd plan restart <plan-id> --yes           # skip the prompt (required when non-interactive)
wd plan restart <plan-id> --force --yes   # also restart an active, starting or paused executor
wd plan restart <plan-id> --backend <id>  # autopilot only: backend for the new manager
```

> **Restart is destructive.** Every agent of the executor (manager and workers,
> or pipeline job agents) is terminated and its worktree removed. Without `--yes`
> the CLI prints what will happen and asks; with no terminal it refuses. The MCP
> tool `restart_plan` is the same operation and is flagged destructive.

What is **kept**: the plan (stays `in_progress`), landed tasks and the landing
list, the integration branch, done pipeline jobs and their handoffs, and every
task branch that has commits beyond the integration branch (open PRs are never
closed). What is **removed**: agent sessions, their worktrees, and task branches
with no commits. Unfinished tasks are reset to `pending` and re-issued to a
**brand-new set of agents** — nothing from the old manager or workers is reused.

Restart works for `in_progress` plans in `autopilot` or `pipeline` mode. It is
refused for `orchestrator_worker` and `manual` plans, for plans that are not
`in_progress`, and for a completed run. Without `--force` it also refuses an
executor that is still healthy (autopilot `active`/`starting`/`paused`; a
pipeline that is running or paused with a working job agent) — `paused` is a
deliberate hold, so use `plan resume` to undo it. `--force` restarts anyway and
leaves the executor active (a paused run is un-paused). Autopilot reuses the
same run id, manager slot and integration branch.

### The restart context

Every new agent gets a **`## Restart context`** section in its prompt (the
manager's digest, each worker's prompt, and each reset pipeline job). It records
why the previous run ended (`operator_stop`, `needs_attention`,
`degraded_backoff`, `operator_force` or `watchdog`), the restart count, the
finished tasks (do **not** redo them), the unfinished tasks with any kept branch
and open PR, and the last 20 decision-journal entries. The instructions are:
continue an unfinished task from its kept branch, reuse its open PR rather than
opening a duplicate, and — only if the kept work is unusable — start again from
the integration branch and close the old PR with a comment. It is stored under
the shared-context key `autopilot.<run_id>.restart_context` (pipeline-only plans:
`plan.<plan_id>.restart_context`), so it survives a daemon restart.

### Guardian triage

Before the guardian (stale heartbeat) or the progress watchdog climbs a rung, a
Fast-Brain diagnosis picks the recovery: `wait` (healthy but slow), a targeted
`nudge`, resolve a stuck prompt, resume a rate limit, redeliver the brief,
`restart`/`rotate` (confidence ≥ 0.8) or hand the stall to the resolver. The model
call runs off the run lock and is applied on a later tick only if the manager and
heal stage have not changed; any failure, timeout or unclear answer runs the plain
ladder step. Keys (all hot-reload): `autopilot.guardian.use_fast_brain` (default
`true`; `false` restores the plain ladder), `max_waits` (3) and `max_wait_total`
(`30m`) bound consecutive `wait` decisions per stall. Every decision is audited as
`autopilot.guardian_diagnosis`.

### Progress watchdog

See *Monitoring a run* above for the full behaviour. In short:
`autopilot.guardian.progress_watchdog_enabled` (default `true`) and
`autopilot.guardian.progress_watchdog_window` (default `2h`) control it; both
hot-reload. After the window with no progress and no working agent it walks the
heal ladder, then parks the run as `no_progress` and points you at
`wd plan restart`. `wd plan show` prints `last_progress` and the watchdog state,
plus `restarts:` (count, last reason, time) once a plan has been restarted.

## Landing a worker branch manually

Landing is daemon-owned: workers open a PR against the integration branch and end with
`wd job done` — they never merge — and warden gates, fixes red CI, and lands the PR for the
manager. `land` is an escape hatch (for when status reports `landing: disabled`). You can
also call it manually to land a specific worker (e.g.
to bypass a stuck gate, or to pre-land a branch you've already reviewed):

```sh
warden autopilot land <agent-id>           # land by agent id
warden autopilot land <branch-name>        # land by branch name
```

The land operation is **idempotent** — landing the same branch twice is a no-op.
A failure prints **one** message (plain sentence + next step, plus the daemon's
detail when present) and exits non-zero. `--json` emits `{kind, detail}`.

| Kind | Meaning | Next step |
|---|---|---|
| `not_found` | Nothing matches that agent or branch | `wd autopilot status` / `wd ls` |
| `not_owned` | The branch exists but is not owned by an autopilot run | Check the run owns it |
| `run_disabled` | The run is paused or stopped | `wd plan resume` |
| `wrong_base` | The PR does not target the integration branch | Retarget the PR |
| `gate_pending` | Checks are still running | Wait, or see the PR checks |
| `gate_red` | Checks failed | Fix the PR |
| `ci_missing` | No CI covers the branch | Add `autopilot/**` to workflow branches, or use local gate |
| `not_mergeable` | Conflicts | Sync the branch |

There is no top-level `wd land`; only `wd autopilot land`.

Over MCP: `land { ticket: "<agent-or-branch>" }`.

---

## Reviewing the integration branch

When the manager has verified the plan's `done_when` criteria it declares the run
done (`autopilot_complete`) and the daemon opens, gates and waits on the final PR
(next section). The run is **complete** only once that PR is merged: the daemon then
writes an in-place `status: complete` marker (plus a `completed_at` timestamp) into
your plan file — preserving your other keys, ordering, and comments — and retains the
ledger. A plan carrying `status: complete` is **skipped by preflight**, so a finished
run is never re-run by mistake on a future plan run or daemon restart. To re-run it,
remove the `status: complete` line (or point the config at a fresh plan file).

### After the final PR is green: awaiting merge

Declaring the run done does not finish the plan. Once `done_when` is verified, the
daemon opens a single final PR (integration → default branch) and gates it. When it
is green the run enters the **`awaiting_merge`** state (`wd autopilot status` shows
"awaiting final PR merge (#n)"):

- The manager and every remaining run agent are terminated and their worktrees
  removed — no agent stays alive just to wait. You are notified once.
- The plan stays **`in_progress`**. Autopilot never merges, approves or closes the
  final PR; merging it is your act.
- The daemon polls the PR every `autopilot.completion.merge_poll_interval` (default
  `2m`, floor `30s`) with no model call. The wait survives a daemon restart.
- **Merged** (merge, squash or rebase) → the run completes, the plan becomes
  `completed`, and the integration branch is deleted locally and on `origin`
  (only when its tip is what was merged; commits pushed afterwards keep it).
- **Head moved**, or the PR became conflicting/dirty/behind → autopilot leaves the
  state, brings integration current or re-gates, and returns to awaiting merge
  without a second notification.
- **Closed without merging** → the run parks as needs-attention (`final_pr_closed`).
  Reopen the PR, run `wd plan resume` to open a new one, or `wd plan stop` to end
  the run and keep the branch.

`wd plan show <id>` prints how the plan ended: the final PR, and the fate of the
integration branch (`deleted`, `kept_unmerged`, `abandoned`, `delete_failed`).


The integration branch for a run (default `autopilot/<plan-name>`; shown in
`warden autopilot status` as `integration_branch`) holds all the merged worker
branches — one merge commit per landed task. Runs already on the legacy
`autopilot/integration` branch keep it across upgrade.

Review the final PR and merge it on GitHub — that merge is what finishes the plan. You can inspect the branch locally first:

```sh
git log autopilot/notifications --oneline   # example per-plan branch
git diff main..autopilot/notifications
```

**The final PR title decides the release bump.** Autopilot titles the final PR
with a conventional-commit subject built from the commits that landed: the type is
the highest-ranked one present (`feat` > `fix` > `perf` > `revert` > `refactor` >
`docs` > `test` > `build` > `ci` > `chore`), the scope is shared by those commits
(omitted if mixed), and a `!` marks a breaking change. Squash-merging it gives
`wd release` a releasable subject; `wd release` also reads the `* type(scope): …`
bullet list in a squash commit body. **Editing the title before you merge changes
the recommended bump** — check it first (`wd release --dry-run` after merging
shows the advice).

The integration branch is **never** merged to `main` by autopilot. That step
always belongs to the operator.

---

## Pausing a repo's runs

```sh
warden plan pause <plan-id>     # pause one run
```

`warden plan pause` is the supported control.
Effective immediately:

- The Controller stops spawning new workers and landing new branches
- In-flight workers **keep running** to completion (they are not terminated)
- The ledger is retained — `warden plan resume <id>` continues where the run left off

Use it any time you want to inspect what workers are doing, or stop a run that
is heading in the wrong direction.

---

## CLI reference

| Command | What it does |
|---|---|
| `warden plan run <id> --mode autopilot` | Start plan execution (canonical lifecycle) |
| `warden plan pause\|resume\|stop <id>` | Control an in-progress plan's executor |
| `warden plan show <id> --watch` | Live status of a running plan: executor state, backoff, integration branch, per-task worker/PR |
| `warden autopilot status [--json]` | Every run's state, manager slot id, integration branch, task summary (includes the former run-list columns) |
| `warden autopilot land <agent-or-branch>` | Land a worker branch into the integration branch |

## MCP tools

| Tool | What it does |
|---|---|
| `run_plan { plan_id, execution_mode }` | Start plan execution |
| `control_plan { plan_id, action }` | Pause, resume, or stop an in-progress plan |
| `autopilot_status` | Return each run's state, manager id, task counts |
| `autopilot_complete` | Manager-only: declare the caller's run complete once `done_when` is met (writes the in-place `status: complete` marker, tears down the manager) |
| `land { ticket: "<agent-or-branch>" }` | Land a worker branch |

## Config hot-reload

The `autopilot` config block **hot-reloads with no daemon restart** — edit
`~/.warden/config.yaml` and the plan/manager/merge template, backend cost ladder,
and guardian heal thresholds re-apply on the next tick. Adding a `plans[]` entry starts it; removing one tears down
its run. Only the guardian tick `interval` still needs a restart. A syntactically
bad edit keeps the last-good config and alerts you. This applies to warden's
whole config file — see [Configuration](/warden/reference/env-vars/).

---

## Known limitations

- **Rate-limit resume and auto-restart are global config toggles** (`rate_limit.auto_resume`,
  `auto_restart`), not per-run overrides via autopilot — configure them in
  `~/.warden/config.yaml` for the backends your manager uses.
- **Rotate (guardian stage 3)** requires more than one free-tier backend to
  exercise meaningfully. With only `antigravity` in the free tier, the guardian
  falls back directly to backoff after a restart fails.
- The manager picks worker backends itself based on the plan; `Controller.SelectWorkerBackend`
  is exposed but the manager is not required to use it.
