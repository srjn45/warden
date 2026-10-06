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
# Recurring agent in the current directory — 5-field cron (@daily etc. supported):
warden schedule create morning-review --cron "0 9 * * 1-5" --role reviewer \
  --prompt "Review yesterday's merged PRs and list follow-ups"

# Single-shot agent in an isolated worktree off a repo, on a chosen AI CLI + model:
warden schedule create release-prep --at 2026-12-01T08:00 --repo ~/dev/app \
  --aicli claude --model sonnet --prompt "Prepare the release notes"

# Fire once, right now:
warden schedule create smoke --now --prompt "Run the smoke checklist"

# Fire a pipeline instead of a single agent (each run gets a timestamped name):
warden schedule create nightly --cron "0 2 * * *" --pipeline ci.yaml
```

### How the agent is started

A scheduled agent starts exactly as `warden start` would with the same flags:

- **Directory.** `--cwd <dir>` launches in an existing directory; the default is
  the directory you ran `create` in, stored as an absolute path. `--repo <path>`
  runs the agent in an isolated worktree off that repo instead (`--branch` picks
  the branch) and replaces the `--cwd` default.
- **Role.** `--role` picks the agent role (see `warden agent role list`); the
  default is `worker` with `--repo`, otherwise `general`.
- **AI CLI and model.** `--aicli <id>` and `--model <id>` (`--model` needs
  `--aicli`); `--tier` lets the quota-balanced resolver choose. Also `--tags`,
  `--permission-mode`, `--auto-restart` and `--project`, as for `warden start`.

A schedule that could never fire — unknown role, missing directory, no prompt — is
rejected at create time with the reason. Agent flags cannot be combined with
`--pipeline`: a pipeline runs as written.

### When it fires

Provide exactly one of `--cron`, `--at` or `--now`. `--at` must be in the future;
a past time is rejected. `--now` fires once as soon as possible. A `--at` time
without a zone, and every cron spec, are interpreted in the **local time of the
machine running the daemon**; prefix a cron spec with `TZ=<zone>` to use another
zone, e.g. `--cron "TZ=Europe/Berlin 0 9 * * *"` (or give `--at` an RFC3339 offset).

## Inspecting & controlling

```sh
warden schedule list                      # NAME STATE WHEN FIRES NEXT LAST
warden schedule show morning-review       # full payload + last run (--spec, --json)
warden schedule run morning-review        # fire once now, to test it
warden schedule edit morning-review --cron "0 8 * * 1-5"
warden schedule disable morning-review    # stop firing (record + history preserved)
warden schedule enable  morning-review    # re-arm: next_run is recomputed from now
warden schedule delete  morning-review --yes   # --yes skips the confirmation prompt
```

**`list`** is a table. STATE is one of `enabled`, `disabled`, `done` (a single-shot
that has fired) or `failed` (a single-shot whose fire failed). NEXT is shown in
local time. If the last run failed, its error is printed beneath the row; a
recurring schedule stays `enabled` and tries again.

**`show`** prints the state and timing, then everything the schedule fires — for an
agent: prompt, directory, repo, branch, role, model, AI CLI and agent name; for a
pipeline: its name and job count (`--spec` adds the stored YAML) — and its last run
with the command to look at it.

**`run`** fires the schedule once, right now, to test it. It does not consume a
single-shot and does not move a recurring schedule's next run, and it works on a
disabled schedule.

**`edit`** changes only the parts you pass. Timing: `--cron` or `--at` (switching
between recurring and single-shot). Payload: the agent options or a new
`--pipeline` file. An empty value (`--prompt ""`) clears an optional field. As on
create, agent flags cannot be combined with `--pipeline`.

**`delete`** asks you to confirm, naming what the schedule fires and its next run.
Pass `--yes`/`-y` to skip the prompt; without a terminal on stdin `--yes` is
required. Only the schedule is removed — agents and pipelines it already started
keep running.

`disable`/`enable` are idempotent and toggle a schedule without losing it —
disable clears `next_run`; enable recomputes it (a cron schedule to its next
occurrence, an `at` schedule to its configured time).

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
or deleted). `warden schedule show` prints the last run.

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
  `get_schedule` / `update_schedule` (`schedule edit`) / `run_schedule`
  (`schedule run`) / `enable_schedule` / `disable_schedule` / `delete_schedule`
  mirror the CLI; create/delete/enable/disable are written to the audit log
  (`schedule_create` / `schedule_delete` / `schedule_enable` / `schedule_disable`).

Schedules persist to an embedded ScrivaDB store under `~/.warden/schedules-db/`
(one record per schedule). On the first daemon launch after upgrading, any legacy
`~/.warden/schedules.json` is imported once and then left in place as a read-only
backup.

## Fixed and removed in this release

- **Fixed:** agent schedules created without `--type` never fired. Every schedule
  the CLI accepts now fires, with the working directory, repo and role chosen the
  same way as `warden start`.
- **Removed:** `warden schedule get` — use `warden schedule show`.
- **Removed:** `--type` on `schedule create` — use `--role` (and `--repo`) instead.
