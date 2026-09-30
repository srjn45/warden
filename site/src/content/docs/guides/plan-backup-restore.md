---
title: Plan backup and restore
description: Portable ScrivaDB Plan bundles for local backup and machine transfer — data location, cadence, and recovery without Git.
---

import { Aside, Steps } from '@astrojs/starlight/components';

Canonical Plans live in ScrivaDB under `<data_dir>/plans-db/` (default `data_dir` is `~/.warden`). Repository files under `plans/**/*.yaml` are optional inert replicas — they are **not** required to list, view, or run a Plan, and they are **not** a recovery path.

## What a Plan backup bundle contains

A versioned JSON bundle (`schema_version: 1`) carries, for each selected Plan:

- Canonical definition (name, goal, constraints, done_when, tasks) plus lifecycle and revision
- Execution history / summaries and task evidence needed for audit
- Typed execution events and attributed notes
- Integrity hashes (`plan_content_hash`, `events_hash`, `notes_hash`, `entry_hash`, `bundle_hash`)

It **excludes** credentials, Hub remote tokens, disposable executor IDs, and worktree cleanup paths. Restore never consults Git or a `plans/` replica.

## CLI

```bash
# Export one Plan (or pass several ids / --all [--project <id>])
wd plan backup export <plan-id> -o plan.bundle.json

# Validate without writing
wd plan backup restore plan.bundle.json --dry-run

# Restore into the daemon's ScrivaDB (idempotent when id+hash+revision match)
wd plan backup restore plan.bundle.json --on-conflict skip
```

`--on-conflict` is `skip` (default), `fail`, or `overwrite`. Identical identity + content hash + revision is always a safe no-op retry (events/notes re-apply idempotently).

API equivalents: `POST /api/v1/plans/export_backup` and `POST /api/v1/plans/restore_backup`. MCP: `export_plan_backup` / `restore_plan_backup`.

## Data location

| Path | Role |
|---|---|
| `<data_dir>/plans-db/` | Canonical Plan store (definitions, lifecycle, execution, events, notes) |
| `plans/**/*.yaml` in a git repo | Optional inert export for review — not backup authority |
| `*.bundle.json` (operator-chosen) | Portable Plan backup produced by `wd plan backup export` |

Configure `data_dir` in `~/.warden/config.yaml` (or the active config). Moving machines means copying the backup bundle (or the whole `plans-db/` directory for a full daemon-state move) — not relying on `git pull` + plan scan.

## Backup cadence

Recommended minimum:

1. **Before risky changes** — daemon upgrades, `factory-reset`, or wiping `data_dir`.
2. **After meaningful Plan progress** — when an execution finishes or you care about audit evidence surviving a laptop loss.
3. **Scheduled** — daily/weekly export of `--all` (or per-project) to encrypted offline storage if Plans are your SoT for in-flight work.

A full filesystem copy of `<data_dir>/plans-db/` is also valid for whole-daemon recovery; the bundle is the Plan-scoped, transferable unit.

## Recovery playbook

<Steps>

1. **On the source machine**, export:

   ```bash
   wd plan backup export --all -o plans-backup.json
   ```

2. **Transfer** `plans-backup.json` to the destination (USB, scp, etc.). Do not expect repository YAML alone to rebuild execution history.

3. **On the destination**, with a running warden daemon pointing at the target `data_dir`:

   ```bash
   wd plan backup restore plans-backup.json --dry-run
   wd plan backup restore plans-backup.json
   wd plan list --project <project-id>
   wd plan show <plan-id>
   wd plan run <plan-id> --mode manual   # or autopilot / pipeline / …
   ```

4. **Conflicts** — if a Plan ID already exists with a different hash/revision, restore reports `conflicted`. Use `--on-conflict overwrite` only when you intend to replace the local record.

</Steps>

<Aside type="caution">
Restore does not recreate agents, worktrees, tmux sessions, or credentials. It restores Plan definition + audit evidence so you can list, view, and start a **new** execution on the destination.
</Aside>
