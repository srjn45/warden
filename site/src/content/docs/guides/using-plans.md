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
wd plan task status <plan-id> analyze done   # pending | in_progress | done | skipped
wd plan complete <plan-id>               # when the mode requires it
wd plan archive <plan-id>
wd plan unarchive <plan-id>             # undo an archive
wd plan delete <plan-id>   # permanently remove a pending or archived plan (-y skips the prompt)
```

## The plan journey

```text
create → edit while pending → run → pause / resume / stop → task status → complete → archive / delete
```

| Stage | Command | Notes |
|---|---|---|
| Create | `wd plan create` | Plan starts `pending` |
| Edit | `wd plan update` / `edit` / `task add\|edit\|rm` | Only while `pending` (409 otherwise) |
| Run | `wd plan run <id> --mode …` | `pending → in_progress` |
| Control | `wd plan pause\|resume\|stop <id>` | Acts on the active executor |
| Track | `wd plan task status <id> <task> <status>` | Any lifecycle state; `skipped` counts as finished |
| Finish | `wd plan complete <id>` | `in_progress → completed` (automatic for `autopilot`/`pipeline`; autopilot finishes when its final PR is **merged**). Refused (422) while the integration branch has commits not on the default branch — merge the PR, or `--abandon-unmerged [--yes]` to complete and keep the branch |
| Retire | `wd plan archive <id>` | → `archived`. Refused (409) while the executor is live (`wd plan stop` first); branches with unmerged commits are kept |
| Restore | `wd plan unarchive <id>` | `archived →` the status it was archived from; an in-progress plan returns with a stopped executor (`wd plan restart <id>` continues it) |
| Remove | `wd plan delete <id>` | Permanent (back up first with `plan backup export`); only `pending`/`archived` plans — `in_progress` and `completed` get a 409, archive first |

<Aside type="caution">
**A running plan's definition cannot be edited.** Once a plan leaves `pending`, goal,
tasks, and constraints are frozen — update/edit/task calls return 409 Conflict. Stop
and archive it, then create a new plan if the definition must change.
</Aside>

`wd plan delete` removes the ScrivaDB record only; any exported YAML replica in the
repository is left untouched. Deletion is available over the CLI and REST
(`DELETE /api/v1/plans/{plan_id}`); there is currently no `delete_plan` MCP tool.

`--name` and `--goal` are required. Tasks form a **DAG**: prefer `--task id@dep1,dep2:prompt`. If you pass two or more tasks with no `after` edges, warden **auto-chains them in flag order**. Cycles and unknown deps are rejected. Optional `--constraint` and `--done-when` may be repeated.

No `plans/` write is required. To publish a reviewable replica later:

```sh
wd plan sync-to-repo <plan-id> --base main
wd plan sync-to-repo <plan-id> --base main --format json   # opt-in JSON replica
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
wd plan show <plan-id> --watch   # live status of a running plan
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

`wd plan show <id> --watch` is the status view for a running plan: executor state, backoff, integration branch, and per-task worker/PR. For autopilot mode there is no separate enable step — `plan run --mode autopilot` is all it takes (the old `wd autopilot enable|on` is a deprecated no-op, and `disable|off` pauses the repo's runs).

Completion is automatic for `autopilot` and `pipeline` when the executor finishes.

## Brain-assisted progress assessment

After restoring a backup without task-level progress (or as a migration aid):

```sh
wd plan assess <plan-id>
```

Opt-in only — never runs on daemon start.

## Legacy cutover and retired commands

`wd plan import-legacy [--report]` is the supported one-shot YAML → ScrivaDB cutover.

`wd plan scan`, `wd plan import`, and `wd plan status` are retired. They remain only as hidden aliases so old scripts keep working, and they cannot affect canonical execution after import. `wd plan sync_to_repo` is likewise a hidden alias of `wd plan sync-to-repo`.

Daemon startup does **not** scan `plans/`.

## Recovering a stuck plan

`wd plan resume` only undoes a pause. For an `in_progress` autopilot or pipeline
plan that was stopped, parked as needs-attention, or stalled with no progress,
run `wd plan restart <id>` — **destructive**: it terminates the executor's agents
and removes their worktrees, keeps landed/done work and branches with commits,
and starts a brand-new set of agents with a `## Restart context`. It asks for
confirmation (`--yes` to skip; required without a terminal); `--force` also
restarts an active, starting or paused executor. Not supported for
`orchestrator_worker`/`manual` plans. Full detail:
[Recovering a stuck plan](/warden/guides/autopilot/#recovering-a-stuck-plan).

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
| `wd plan sync-to-repo <id> --base <ref> [--format yaml\|json]` | Optional inert replica PR (YAML default) |
| `wd plan hub-sync push\|pull\|discover [<id>] --scope <project-id>` | Explicit opt-in Hub envelope sync |
| `wd plan backup export\|restore …` | Portable ScrivaDB bundle |
| `wd plan import-legacy [--report]` | Explicit legacy YAML cutover |
| `wd plan task status <id> <task> <status>` | Set one task's progress |
| `wd plan delete <id>` | Permanently delete a plan (not while `in_progress`) |
| `wd plan complete` / `archive` / `unarchive` / `run` / `pause\|resume\|stop` | Lifecycle + execution |
| `wd plan restart <id> [--force] [--backend <id>] [--yes]` | **Destructive.** Restart a stopped/parked/stuck autopilot or pipeline run with fresh agents |
| `wd plan assess <id>` | Brain-assisted task progress |

## TUI

In the cockpit (`wd tui`), local plans appear above agents in the project tree,
grouped by status. With a Hub provider configured, explicit Discover results
also appear per project in a read-only **Remote Plans** section with a count
badge; only pending and in-progress remote plans are shown. Detail for local
plans is ScrivaDB-backed. Keybindings: `a` archive · `A` assess · `r` run ·
`enter` detail.
