---
title: Autopilot — autonomous agent runs
description: Adoption walkthrough — init, cost-tier config, enable/disable, landing branches, and how to stay safe running agents unattended.
---

import { Aside } from '@astrojs/starlight/components';

<Aside type="caution" title="Unattended operation is inherently risky">
When autopilot is enabled, a "brain" agent orchestrates worker agents **without
waiting for human input**. Workers write code, open PRs, and merge branches into
the integration branch — autonomously. You should understand the mitigations
before enabling:

- **Kill switch:** `warden autopilot off` stops new spawns and landings
  immediately (in-flight workers keep running). Use it any time you need to
  regain control.
- **Integration-branch boundary:** workers never merge to `main` directly —
  all changes land in `autopilot/integration` first, where you review them
  before deciding to fast-forward `main`.
- **Audit log:** every autopilot action (brain spawn, worker spawn, land, heal)
  is written to `warden audit log` — a permanent, append-only record of what
  ran.
</Aside>

Autopilot lets warden run a **goal-directed, long-lived agent loop** over your
codebase. You describe what you want in a plan file, enable autopilot, and warden
takes care of the rest: spawning a "brain" agent that breaks the goal into tasks,
delegates each task to worker agents in isolated worktrees, gates their branches
through CI, and lands them into an integration branch — healing itself when stuck,
and escalating to cheaper backends when rate-limited.

For the underlying design — brain, ledger, guardian, cost-tier ladder — see
[Autopilot concepts](../concepts/autopilot).

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
tasks: []           # leave empty to let the brain decompose the goal automatically
```

**`~/.warden/config.yaml`** — updated with an `autopilot` block:

```yaml
autopilot:
  enabled: false
  plan_file: /path/to/my-repo/autopilot.plan.yaml
  integration_branch: autopilot/integration
  gate_mode: ci            # ci | local | auto (default: auto picks ci when available)
```

Commit `autopilot.plan.yaml` to your repo so the brain can read it from its
worktree.

---

## Step 2 — edit your plan file

Open `autopilot.plan.yaml` and fill in the goal. The brain decomposes the goal
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

The plan file is **owner-editable mid-flight** — the brain re-reads it on each
planning tick. You can add tasks or change constraints while a run is active.

---

## Step 3 — configure the cost tier (optional)

By default, the brain picks the cheapest available backend. To explicitly configure
which backends autopilot may use, edit `~/.warden/config.yaml`:

```yaml
autopilot:
  # allow_tiers controls which cost tiers the brain may use.
  # Values: free, subscription, gated_ppu (pay-per-use — requires explicit opt-in)
  allow_tiers: [free, subscription]    # default: [free, subscription]

  # backend_preference: explicitly order the backends to try (optional)
  # backend_preference: [antigravity, claude, codex]
```

| Tier | Backends | Notes |
|---|---|---|
| `free` | `antigravity` | Google-hosted free tier; no billing |
| `subscription` | `claude`, `codex` | Your existing plan |
| `gated_ppu` | API-billed backends | Requires explicit opt-in |

If you only want the brain to use the free tier (e.g. to test autopilot at zero
additional cost), set `allow_tiers: [free]`.

---

## Step 4 — enable autopilot

```sh
warden autopilot on
```

This runs a **preflight check** before enabling. The preflight surfaces every
problem that would stall an unattended run — now, while you're present — and
prints actionable errors if anything is missing:

```
✗ plan file not found: autopilot.plan.yaml
✗ integration branch does not exist: autopilot/integration
✗ no authenticated backend available
hint: run `warden autopilot init` to scaffold a plan file and config block
```

Fix any reported issues and re-run `warden autopilot on`. When the preflight
passes, the brain is spawned and the run enters `active` state.

---

## Monitoring a run

```sh
warden autopilot status          # run state, brain id, landed/pending task counts
warden ls                        # shows the brain + all worker agents
warden status <brain-id>         # full brain detail + events
warden tail <brain-id>           # recent brain output
warden audit log                 # full append-only audit trail of every action
```

The TUI cockpit (`warden tui`) shows the brain and its workers as a nested
sub-tree under the run. The web dashboard shows an **Autopilot** panel when a run
is active. The TUI header has a status badge (press `ctrl+a` to toggle autopilot
on/off without leaving the cockpit).

---

## Landing a worker branch manually

The brain calls `warden land` automatically when a worker finishes and its PR
is gate-green. You can also call it manually to land a specific worker (e.g.
to bypass a stuck gate, or to pre-land a branch you've already reviewed):

```sh
warden land <agent-id>           # land by agent id
warden land <branch-name>        # land by branch name
```

The land operation is **idempotent** — landing the same branch twice is a no-op.
It fails with an error if:

- The branch is not autopilot-owned (ownership guard)
- The configured gate is not green (`--gate-mode=local` bypasses CI and uses the
  local `.warden/check.yml` checks instead)

Over MCP: `land { ticket: "<agent-or-branch>" }`.

---

## Reviewing the integration branch

When all tasks are landed, autopilot marks the run `complete` and tears down the
brain. The integration branch (`autopilot/integration` by default) holds all the
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
warden autopilot off
```

Effective immediately, at any state:

- The Controller stops spawning new workers and landing new branches
- In-flight workers **keep running** to completion (they are not terminated)
- The brain is terminated gracefully
- The ledger is retained — `warden autopilot on` continues from where the run
  left off

Use the kill switch any time you want to pause the run, inspect what workers are
doing, or abort a run that is heading in the wrong direction.

---

## CLI reference

| Command | What it does |
|---|---|
| `warden autopilot init` | Scaffold `autopilot.plan.yaml` + config block |
| `warden autopilot on` | Enable autopilot (runs preflight first) |
| `warden autopilot off` | Disable autopilot — the kill switch |
| `warden autopilot status` | Show current run state, brain id, task summary |
| `warden land <agent-or-branch>` | Land a worker branch into the integration branch |

## MCP tools

| Tool | What it does |
|---|---|
| `set_autopilot { enabled: true\|false }` | Enable or disable autopilot (the kill switch) |
| `autopilot_status` | Return current run state, brain id, task counts |
| `land { ticket: "<agent-or-branch>" }` | Land a worker branch |

---

## Known limitations

- **Rate-limit resume and auto-restart are global config toggles** (`rate_limit.auto_resume`,
  `auto_restart`), not per-run overrides via autopilot — configure them in
  `~/.warden/config.yaml` for the backends your brain uses.
- **Rotate (guardian stage 3)** requires more than one free-tier backend to
  exercise meaningfully. With only `antigravity` in the free tier, the guardian
  falls back directly to backoff after a restart fails.
- The brain picks worker backends itself based on the plan; `Controller.SelectWorkerBackend`
  is exposed but the brain is not required to use it.
