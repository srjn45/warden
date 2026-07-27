---
title: Scheduling agents & pipelines
description: Fire an agent spawn or a pipeline on the daemon's own cron/at timer — no external crontab.
---

`warden schedule` fires an agent spawn **or** a pipeline on the daemon's own
timer, through the same internal seams the `/spawn` and pipeline routes use — no
external crontab.

:::note[Opt-in]
The scheduler is **off by default**. Set `scheduler_enabled: true` in
`~/.warden/config.yaml` and keep the daemon running — schedules only fire while the
daemon is up.
:::

## Creating schedules

```sh
# Recurring agent spawn — 5-field cron (@daily etc. supported):
warden schedule create daily-review --cron "0 9 * * *" \
  --type pr-review --repo . --prompt "Review yesterday's merged PRs"

# Single-shot spawn — RFC3339 or 2006-01-02T15:04 (local time):
warden schedule create launch --at 2026-06-27T09:00 \
  --prompt "Kick off the release checklist"

# Fire a pipeline instead of a single agent (each run gets a timestamped name):
warden schedule create nightly --cron "0 2 * * *" --pipeline ci.yaml
```

## Inspecting & removing

```sh
warden schedule list              # kind (cron/at), mode (agent/pipeline), spec, enabled, next run, last error
warden schedule delete daily-review
```

## Behaviour

- **No backfill.** On daemon startup each next-fire is recomputed from the wall
  clock: a cron schedule resumes at its next *future* occurrence (a run missed while
  the daemon was down is not replayed), and a past-due single-shot fires once.
- **Fail-soft.** A fire error is recorded in the schedule's `last_error` and logged;
  it never crashes the reconcile loop or stops other schedules firing.
- **Fully driveable over MCP.** `create_schedule` / `list_schedules` / `delete_schedule`
  mirror the CLI; create/delete are written to the audit log (`schedule_create` /
  `schedule_delete`).

Schedules persist to an embedded ScrivaDB store under `~/.warden/schedules-db/`
(one record per schedule). On the first daemon launch after upgrading, any legacy
`~/.warden/schedules.json` is imported once and then left in place as a read-only
backup.
