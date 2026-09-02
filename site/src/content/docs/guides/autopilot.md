---
title: Autopilot — autonomous agent runs
description: Adoption walkthrough — init, cost-tier config, enable/disable, landing branches, and how to stay safe running agents unattended.
---

import { Aside } from '@astrojs/starlight/components';

<Aside type="caution" title="Unattended operation is inherently risky">
When autopilot is enabled, a **manager** agent drives a fleet of worker agents
**without waiting for human input**. Workers write code, open PRs, and merge
branches into the integration branch — autonomously. You should understand the
mitigations before enabling:

- **Kill switch:** `warden autopilot disable` stops new spawns and landings
  immediately (in-flight workers keep running). Use it any time you need to
  regain control.
- **Integration-branch boundary:** workers never merge to `main` directly —
  all changes land in `autopilot/integration` first, where you review them
  before deciding to fast-forward `main`.
- **Audit log:** every autopilot action (manager spawn, worker spawn, land, heal)
  is written to `warden inspect audit` — a permanent, append-only record of what
  ran.
</Aside>

Autopilot lets warden run a **goal-directed, long-lived agent loop** over your
codebase. You describe what you want in a plan file, enable autopilot, and warden
takes care of the rest: spawning a **manager** agent that breaks the goal into
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

Before enabling autopilot, make sure:

- `warden daemon` is running and healthy (`warden doctor`)
- At least one agent backend is authenticated (`claude --version` for the default
  Claude backend; or configure an alternative in `~/.warden/config.yaml`)
- Your repository has a `main` branch and GitHub Actions CI (or a local CI
  configured in `.warden/check.yml`) — the `gated` step verifies the gate before
  landing
- `gh` is authenticated (`gh auth status`) — autopilot opens PRs and checks CI
  status via the GitHub CLI

---

## Step 1 — scaffold with `warden autopilot init`

Run `init` inside the repo you want autopilot to drive:

```sh
cd /path/to/my-repo
warden autopilot init
```

This creates two files (without overwriting either if they already exist):

**`autopilot.plan.yaml`** — edit this to describe your goal:

```yaml
version: 1
goal: "Describe your goal here"
constraints:
  - "keep all changes behind a feature flag"
tasks: []           # leave empty to let the manager decompose the goal automatically
```

**`~/.warden/config.yaml`** — updated with an `autopilot` block:

```yaml
autopilot:
  enabled: false
  plan_file: /path/to/my-repo/autopilot.plan.yaml
  integration_branch: autopilot/integration
  gate_mode: ci            # ci | local | auto (default: auto picks ci when available)
```

Commit `autopilot.plan.yaml` to your repo so the manager can read it from its
worktree.

---

## Step 2 — edit your plan file

Open `autopilot.plan.yaml` and fill in the goal. The manager decomposes the goal
into tasks automatically if you leave the `tasks:` list empty. Or provide coarse
tasks yourself to guide decomposition:

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

The plan file is **owner-editable mid-flight** — the manager re-reads it on each
planning tick. You can add tasks or change constraints while a run is active.

---

## Step 3 — configure the cost tier (optional)

By default, the manager picks the **cheapest available backend**. The cost-tier ladder
is now **derived from the [backend registry](/warden/guides/backend-registry/)** — a
backend's tier is whatever you set with `warden backend tier`, and only **installed,
enabled, non-`local`** backends are eligible. So you steer autopilot's spending by
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

## Step 4 — enable autopilot

The switch is **per-repository**. Run inside the repo you want to drive:

```sh
warden autopilot enable
```

This enables **only the current repository** (other repos are unaffected) and
runs a **preflight check** before enabling. Add `--repo <root>` to target a
different repository. The preflight surfaces every problem that would stall an
unattended run — now, while you're present — and prints actionable errors if
anything is missing:

```
✗ plan file not found: autopilot.plan.yaml
✗ integration branch does not exist: autopilot/integration
✗ no authenticated backend available
hint: run `warden autopilot init` to scaffold a plan file and config block
```

Fix any reported issues and re-run `warden autopilot enable`. When the preflight
passes, the manager is spawned and the run enters `active` state, and the repo is
**persisted as enabled** — so it comes back up automatically if the daemon
restarts. Enable more repos the same way; each is tracked independently.

---

## Monitoring a run

```sh
warden autopilot status          # enabled repos + run state, manager id, task counts
warden ls                        # shows the manager + all worker agents
warden status <manager-id>       # full manager detail + events
warden agent tail <manager-id>         # recent manager output
warden inspect audit                 # full append-only audit trail of every action
```

The TUI cockpit (`warden tui`) shows the manager and its workers as a nested
sub-tree under the run. The web dashboard shows an **Autopilot** panel when a run
is active. The TUI header has a status badge (press `ctrl+a` to toggle autopilot
on/off without leaving the cockpit).

---

## Landing a worker branch manually

The manager calls `warden autopilot land` automatically when a worker finishes and its PR
is gate-green. You can also call it manually to land a specific worker (e.g.
to bypass a stuck gate, or to pre-land a branch you've already reviewed):

```sh
warden autopilot land <agent-id>           # land by agent id
warden autopilot land <branch-name>        # land by branch name
```

The land operation is **idempotent** — landing the same branch twice is a no-op.
It fails with an error if:

- The branch is not autopilot-owned (ownership guard)
- The configured gate is not green (`--gate-mode=local` bypasses CI and uses the
  local `.warden/check.yml` checks instead)

Over MCP: `land { ticket: "<agent-or-branch>" }`.

---

## Reviewing the integration branch

When the manager has verified the plan's `done_when` criteria, it marks the run
**complete**: the daemon writes an in-place `status: complete` marker (plus a
`completed_at` timestamp) into your plan file — preserving your other keys,
ordering, and comments — tears down the manager (in-flight workers keep running),
and retains the ledger. A plan carrying `status: complete` is **skipped by
preflight**, so a finished run is never re-run by mistake on a future enable or
daemon restart. To re-run it, remove the `status: complete` line (or point the
config at a fresh plan file).

The integration branch (`autopilot/integration` by default) holds all the
merged worker branches — one merge commit per landed task.

Review the branch, then fast-forward `main` when you're satisfied:

```sh
git log autopilot/integration --oneline   # inspect landed commits
git diff main..autopilot/integration      # full diff

# fast-forward main (after your review)
git checkout main
git merge --ff-only autopilot/integration
git push
```

The integration branch is **never** merged to `main` automatically. That step
always belongs to the operator.

---

## Kill switch

```sh
warden autopilot disable              # disable the current repo
warden autopilot disable --repo <root>  # disable a specific repo
```

Disables the **current repository** (or `--repo <root>`); other enabled repos
keep running. Effective immediately, at any state:

- The Controller stops spawning new workers and landing new branches
- In-flight workers **keep running** to completion (they are not terminated)
- The manager is terminated gracefully
- The ledger is retained — `warden autopilot enable` continues from where the run
  left off

Use the kill switch any time you want to pause the run, inspect what workers are
doing, or abort a run that is heading in the wrong direction.

---

## CLI reference

| Command | What it does |
|---|---|
| `warden autopilot init` | Scaffold `autopilot.plan.yaml` + config block |
| `warden autopilot enable [--repo <root>]` | Enable autopilot for this repo (runs preflight first) |
| `warden autopilot disable [--repo <root>]` | Disable autopilot for this repo — the kill switch |
| `warden autopilot status` | Show enabled repos + each run's state, manager id, task summary |
| `warden autopilot land <agent-or-branch>` | Land a worker branch into the integration branch |

## MCP tools

| Tool | What it does |
|---|---|
| `set_autopilot { enabled: true\|false, repo? }` | Enable or disable autopilot for a repo (the kill switch); `repo` defaults to the daemon's working directory |
| `autopilot_status` | Return enabled repos + each run's state, manager id, task counts |
| `autopilot_complete` | Manager-only: declare the caller's run complete once `done_when` is met (writes the in-place `status: complete` marker, tears down the manager) |
| `land { ticket: "<agent-or-branch>" }` | Land a worker branch |

## Config hot-reload

The `autopilot` config block **hot-reloads with no daemon restart** — edit
`~/.warden/config.yaml` and the plan/manager/merge template, backend cost ladder,
and guardian heal thresholds re-apply on the next tick, with the per-repo enabled
set left untouched. Adding a `plans[]` entry starts it; removing one tears down
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
