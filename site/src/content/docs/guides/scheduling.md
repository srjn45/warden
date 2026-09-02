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

## Inspecting & controlling

```sh
warden schedule list              # kind (cron/at), mode (agent/pipeline), spec, enabled, next run, last error
warden schedule show daily-review  # one schedule + its last-run session id and outcome
warden schedule disable daily-review   # stop firing (record + history preserved)
warden schedule enable  daily-review   # re-arm: next_run is recomputed from now
warden schedule delete  daily-review
```

`disable`/`enable` are idempotent and toggle a schedule without losing it —
disable clears `next_run`; enable recomputes it (a cron schedule to its next
occurrence, an `at` schedule to its configured time, which fires on the next tick
if already past).

## Following a scheduled run

Every session a schedule fires carries a **`schedule_id`** (and `schedule_name`)
back-reference — set on agent-mode spawns directly, and inherited by a scheduled
pipeline's job sessions. It appears everywhere sessions surface: `GET /sessions`,
`GET /sessions/{id}`, and the live SSE event stream. A client can therefore tag a
running session as schedule-origin, keep it out of the plain agents list, and jump
straight into the live run's terminal — all by filtering that one field.

The schedule record itself also keeps a **durable** pointer to its most recent
run: `last_run_session_id` plus `last_run_status` (refreshed from the run's live
status while its session exists, and preserved even after the session is rotated
or deleted). `warden schedule show` prints both.

Daemons that support this end-to-end advertise the **`scheduled-agents`** flag in
`GET /api/v1/capabilities`, so a client can feature-detect it the same way it
detects `terminal-sessions`.

## Behaviour

- **No backfill.** On daemon startup each next-fire is recomputed from the wall
  clock: a cron schedule resumes at its next *future* occurrence (a run missed while
  the daemon was down is not replayed), and a past-due single-shot fires once.
- **Fail-soft.** A fire error is recorded in the schedule's `last_error` and logged;
  it never crashes the reconcile loop or stops other schedules firing.
- **Fully driveable over MCP.** `create_schedule` / `list_schedules` /
  `get_schedule` / `enable_schedule` / `disable_schedule` / `delete_schedule`
  mirror the CLI; create/delete/enable/disable are written to the audit log
  (`schedule_create` / `schedule_delete` / `schedule_enable` / `schedule_disable`).

Schedules persist to an embedded ScrivaDB store under `~/.warden/schedules-db/`
(one record per schedule). On the first daemon launch after upgrading, any legacy
`~/.warden/schedules.json` is imported once and then left in place as a read-only
backup.
