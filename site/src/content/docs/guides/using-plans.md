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
wd plan delete <plan-id>   # permanently remove (refused while in_progress)
```

`--name` and `--goal` are required. Tasks form a **DAG**: prefer `--task id@dep1,dep2:prompt`. If you pass two or more tasks with no `after` edges, warden **auto-chains them in flag order**. Cycles and unknown deps are rejected. Optional `--constraint` and `--done-when` may be repeated.

No `plans/` write is required. To publish a reviewable replica later:

```sh
wd plan sync_to_repo <plan-id> --base main
wd plan sync_to_repo <plan-id> --base main --format json   # opt-in JSON replica
```

YAML is the default. JSON uses the same envelope and is never execution authority.

## Modifying a pending plan

While a plan is **`pending`**, you can change its definition in ScrivaDB. Non-pending
plans reject definition edits with **HTTP 409 Conflict**.

```sh
# Patch metadata (and/or apply a YAML file)
wd plan update <plan-id> --goal "Updated goal" --constraint "Fast turnaround"
wd plan update <plan-id> --file ./my-plan.yaml

# Interactive editor ($EDITOR, else nano/vi) — validates DAG before save
wd plan edit <plan-id>

# Granular task-DAG mutations
wd plan task add <plan-id> --id t2 --prompt "Second task" --after t1
wd plan task edit <plan-id> --id t1 --prompt "Refined first task"
wd plan task rm <plan-id> t2
```

`--file` is parsed with the same DAG rules as create (cycles and unknown deps
rejected). Optimistic concurrency uses the plan's current revision on update/edit;
task subcommands accept optional `--expected-revision`.

## Optional Hub sync and remote discovery

Hub sync is an opt-in transport for revision envelopes, not a replacement for
Git, CI, or the canonical local ScrivaDB record. Configure `plan_sync.provider:
hub`, `plan_sync.hub_url`, and a token (prefer `WARDEN_PLAN_SYNC_TOKEN`, which
overrides `plan_sync.token`), then make an explicit operator request:

```sh
wd plan hub-sync push <plan-id> --scope <project-id>
wd plan hub-sync pull --scope <project-id>
wd plan hub-sync discover --scope <project-id>
```

`--status` is repeatable on Pull and Discover. With no status filter, Discover
uses `pending` and `in_progress`. The matching MCP tools are `hub_sync_push`,
`hub_sync_pull`, and `hub_sync_discover`. Local is the default provider: it
makes no network calls, returns no remote entries, and never starts background
replication.

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
| `wd plan update <id> [--file] [--name] [--goal] [--constraint] [--done-when]` | Patch pending definition (409 if not pending) |
| `wd plan edit <id>` | Edit pending definition in `$EDITOR` |
| `wd plan task add\|edit\|rm …` | Granular task-DAG mutations on pending plans |
| `wd plan show <id> [--json]` | Show canonical detail |
| `wd plan sync_to_repo <id> --base <ref> [--format yaml\|json]` | Optional inert replica PR (YAML default) |
| `wd plan hub-sync push\|pull\|discover [<id>] --scope <project-id>` | Explicit opt-in Hub envelope sync |
| `wd plan backup export\|restore …` | Portable ScrivaDB bundle |
| `wd plan import-legacy [--report]` | Explicit legacy YAML cutover |
| `wd plan scan …` / `import` / `status` | Deprecated migration aids |
| `wd plan done` / `complete` / `archive` / `run` / `pause\|resume\|stop` | Lifecycle + execution |
| `wd plan assess <id>` | Brain-assisted task progress |

## TUI

In the cockpit (`wd tui`), local plans appear above agents in the project tree,
grouped by status. With a Hub provider configured, explicit Discover results
also appear per project in a read-only **Remote Plans** section with a count
badge; only pending and in-progress remote plans are shown. Detail for local
plans is ScrivaDB-backed. Keybindings: `a` archive · `A` assess · `r` run ·
`enter` detail. (`s` scan remains as a deprecated migration aid.)
