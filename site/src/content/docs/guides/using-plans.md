---
title: Using plans
description: How to scan, run, and recover plans — the four execution modes, brain-assisted progress assessment, and cross-machine reinstall recovery.
---

import { Aside, Steps } from '@astrojs/starlight/components';

Plans are YAML files stored in your repo under `plans/{pending,in_progress,completed,archived}/`. The daemon scans those directories, tracks each plan's execution state (links to autopilot runs, pipelines, and task progress), and shows plans in the TUI project tree above agents. The **directory the file sits in is the plan's status** — moving the file is a state transition.

## Directory layout

```
plans/
  pending/
    feature-x.yaml
    feature-y.yaml
  in_progress/
    brain-consult.yaml
  completed/
    old-feature.yaml
  archived/
    stale-plan.yaml
```

Status is encoded by directory, so the full plan state is always readable from `git log` and visible to every team member who clones the repo — no warden daemon required to see which plans are active.

## First scan

After adding plan files to your repo (or cloning one that already has them), tell the daemon to scan:

```sh
wd plan scan
```

The daemon walks all four subdirectories, derives each plan's stable ID from its name, and upserts records into its ScrivaDB `plans` collection. Status is inferred from the directory. Running scan again is safe — it only updates `FilePath` and `Status`; it never overwrites execution links or task progress.

### Migrating flat plans

If your repo has existing `plans/*.yaml` files (flat, not in subdirectories), migrate them in one step:

```sh
wd plan scan --migrate-flat
```

This `git mv`s every flat file into `plans/pending/`, commits, and then scans.

## Listing and inspecting plans

```sh
wd plan list                          # all plans for this project
wd plan list --status in_progress     # filter by status
wd plan list --json                   # machine-readable

wd plan show <plan-id>                # full detail: file, status, mode, links, timestamps
wd plan show <plan-id> --json
```

The plan ID is the stable `plan-<8hex>` identifier printed by `wd plan list`.

## Importing a plan

To bring in a plan YAML from outside the project:

```sh
wd plan import path/to/feature-x.yaml
```

This copies the file into `plans/pending/` and triggers a scan automatically.

## Transitioning status

`wd plan status` performs the `git mv`, creates a commit, and updates the DB record atomically:

```sh
wd plan status <plan-id> in_progress    # start a plan (manual mode)
wd plan status <plan-id> completed      # mark done
wd plan status <plan-id> archived       # de-prioritise
wd plan archive <plan-id>               # shorthand for → archived
```

Valid statuses: `pending` · `in_progress` · `completed` · `archived`

<Aside>
Any status can transition to `archived`. `completed` and `archived` cannot move back to `in_progress` without an explicit reset — this is intentional to prevent accidental re-runs.
</Aside>

## Running a plan

`wd plan run` links a plan to an execution entity, moves it to `in_progress`, and starts work according to the chosen **execution mode**:

```sh
wd plan run <plan-id> --mode autopilot            # fully autonomous
wd plan run <plan-id> --mode pipeline             # task-per-pipeline-job
wd plan run <plan-id> --mode orchestrator_worker  # human-gated workers
wd plan run <plan-id> --mode manual               # state tracking only
```

### Execution modes

| Mode | What happens |
|---|---|
| `autopilot` | Registers an autopilot run against the plan; the manager drives workers autonomously and moves the plan to `completed/` when done. |
| `pipeline` | Creates a DAG pipeline where each YAML task becomes a job; moves to `completed/` when the pipeline finishes. |
| `orchestrator_worker` | Spawns an orchestrator agent with the plan as context; each worker requires a human approval gate. Completion is manual (`wd plan status <id> completed`). |
| `manual` | git-mv to `in_progress/` only — state tracking with no execution entity. You drive all prompting. |

Completion detection is automatic for `autopilot` and `pipeline` modes: the daemon watches for the run/pipeline completion event and performs the git-mv to `plans/completed/` plus the DB update without operator intervention.

## Brain-assisted progress assessment

After a reinstall or DB wipe, `wd plan assess` uses a brain model to reconstruct task-level progress from git history and open PR metadata:

```sh
wd plan assess <plan-id>
```

This reads the plan YAML's task list, feeds recent `git log`, open PR titles, and any existing `task_progress` into the Consultor, and writes the brain's assessment back to `task_progress` in the DB record. It is **opt-in** — never run automatically, since spawning a brain on every daemon start would be expensive.

To assess all `in_progress` plans in one pass:

```sh
wd plan scan --assess
```

## Cross-machine recovery

| Scenario | Recovery |
|---|---|
| Same machine, DB intact | Normal operation |
| Same machine, DB wiped | `wd plan scan` re-seeds all plans with correct status from the directory layout |
| New machine / reinstall | `git pull` → daemon start auto-scans → plans appear with correct status |
| Missing task-level progress | `wd plan assess <plan-id>` reconstructs from git/PRs |
| Full DB backup + restore | `wd snapshot restore` restores ScrivaDB including execution links |

<Steps>
1. **After a reinstall:** run `wd plan scan` — this seeds all plans from the directory layout in your repo. Status (pending/in_progress/completed/archived) is fully recovered from git.
2. **For in-progress plans:** run `wd plan assess <plan-id>` for each active plan whose task progress matters. The brain reads recent commits and PR titles to reconstruct which tasks are done.
3. **Verify:** `wd plan list` should now show all plans with correct statuses.
</Steps>

## Command reference

| Command | What it does |
|---|---|
| `wd plan list [--status <s>] [--json]` | List plans (optionally filtered by status) |
| `wd plan show <id> [--json]` | Show full detail for one plan |
| `wd plan import <file>` | Copy a YAML into `plans/pending/` and scan |
| `wd plan scan [--migrate-flat] [--assess]` | Walk `plans/` directories and upsert records |
| `wd plan status <id> <new-status>` | git mv + commit + DB update |
| `wd plan archive <id>` | Shorthand for `status → archived` |
| `wd plan assess <id>` | Brain-assisted task progress reconstruction |
| `wd plan run <id> --mode <mode>` | Start execution in the given mode |

## TUI

In the cockpit TUI (`wd tui`), plans appear **above agents** in the project tree, grouped by status:

```
▼ my-project
  ▼ Plans
    ▶ In Progress  (1)
      · brain-consult
    ▶ Pending      (2)
      · feature-x
      · feature-y
    ▶ Completed    (3)
    ▶ Archived     (hidden by default — expand with key)
  ▼ Agents
    · agent-1
```

Keybindings in plan context: `a` archive · `s` scan · `A` assess (brain) · `r` run (mode picker) · `enter` open detail pane.
