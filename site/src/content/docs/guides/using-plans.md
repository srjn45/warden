---
title: Using plans
description: Create, run, and complete ScrivaDB-canonical Plans — DB-native workflow, optional replica export, and deprecated migration aids.
---

import { Aside, Steps } from '@astrojs/starlight/components';

Plans are **canonical ScrivaDB records**. You do not need a `plans/` directory to create, list, run, or complete them. Repository YAML/JSON is an optional inert export for review.

Operator playbooks (legacy import, replica PR, backup restore, export conflicts): [Plans migration](/warden/guides/plans-migration/).

## Fresh DB-native use

```sh
wd plan create --name feature-x --goal "ship it" \
  --task 'analyze:scope the change' \
  --task 'implement@analyze:write the code' \
  --task 'review@implement:open the PR'
wd plan list
wd plan show <plan-id>
wd plan run <plan-id> --mode autopilot   # or pipeline | orchestrator | manual
wd plan done <plan-id> analyze           # only ready tasks can be marked done
wd plan complete <plan-id>               # when the mode requires it
wd plan archive <plan-id>
```

`--name` and `--goal` are required. Tasks form a **DAG**: prefer `--task id@dep1,dep2:prompt`. If you pass two or more tasks with no `after` edges, warden **auto-chains them in flag order**. Cycles and unknown deps are rejected. Optional `--constraint` and `--done-when` may be repeated.

No `plans/` write is required. To publish a reviewable replica later:

```sh
wd plan sync_to_repo <plan-id> --base main
wd plan sync_to_repo <plan-id> --base main --format json   # opt-in JSON replica
```

YAML is the default. JSON uses the same envelope and is never execution authority.

## Listing and inspecting

```sh
wd plan list                          # all plans for this project
wd plan list --status in_progress
wd plan list --json
wd plan show <plan-id>
wd plan show <plan-id> --json
```

Detail comes from ScrivaDB (goal, tasks, revision, export status, execution). Repository YAML/JSON replicas are never read for this view.

## Running a plan

```sh
wd plan run <plan-id> --mode autopilot
wd plan run <plan-id> --mode pipeline
wd plan run <plan-id> --mode orchestrator
wd plan run <plan-id> --mode manual
wd plan pause|resume|stop <plan-id>
```

| Mode | What happens |
|---|---|
| `autopilot` | Live `Autopilot` + manager Agent; Plan owns task state and events |
| `pipeline` | DAG pipeline `P:<plan-name>` from the canonical task DAG |
| `orchestrator_worker` | Agent `O:<plan-name>`; complete with `wd plan complete` |
| `manual` | Agent `M:<plan-name>`; complete with `wd plan complete` |

Completion is automatic for `autopilot` and `pipeline` when the executor finishes.

## Brain-assisted progress assessment

After restoring a backup without task-level progress (or as a migration aid):

```sh
wd plan assess <plan-id>
```

Opt-in only — never runs on daemon start.

## Deprecated migration aids (one release)

These remain callable so old scripts keep working. Their help and responses state that they **cannot affect canonical execution after import**. Prefer the DB-native commands above.

| Command | Status |
|---|---|
| `wd plan scan [--migrate-flat] [--assess]` | Deprecated — upserts stubs only; does not reseed Status for Plans with a definition |
| `wd plan import <file>` | Deprecated — copies into `plans/pending/` + scan |
| `wd plan status <id> <status>` | Deprecated — prefer `run` / `complete` / `archive` |
| `wd plan import-legacy [--report]` | Supported cutover — explicit one-shot YAML → ScrivaDB |

Daemon startup does **not** scan `plans/`.

## Recovery

Prefer Plan backup bundles (definition + audit; no Git required):

```sh
wd plan backup export --all -o plans-backup.json
wd plan backup restore plans-backup.json --dry-run
wd plan backup restore plans-backup.json
```

See [Plan backup and restore](/warden/guides/plan-backup-restore/).

## Command reference

| Command | What it does |
|---|---|
| `wd plan list [--status <s>] [--json]` | List ScrivaDB plans |
| `wd plan create --name <n> --goal <g> [--task id:prompt]` | Create pending Plan in ScrivaDB |
| `wd plan show <id> [--json]` | Show canonical detail |
| `wd plan sync_to_repo <id> --base <ref> [--format yaml\|json]` | Optional inert replica PR (YAML default) |
| `wd plan backup export\|restore …` | Portable ScrivaDB bundle |
| `wd plan import-legacy [--report]` | Explicit legacy YAML cutover |
| `wd plan scan …` / `import` / `status` | Deprecated migration aids |
| `wd plan done` / `complete` / `archive` / `run` / `pause\|resume\|stop` | Lifecycle + execution |
| `wd plan assess <id>` | Brain-assisted task progress |

## TUI

In the cockpit (`wd tui`), plans appear above agents in the project tree, grouped by status. Detail is ScrivaDB-backed. Keybindings: `a` archive · `A` assess · `r` run · `enter` detail. (`s` scan remains as a deprecated migration aid.)
