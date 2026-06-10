---
title: Pipelines (DAG)
description: Define a DAG of dependent agent jobs in YAML and let the daemon run them — outputs flow downstream automatically.
---

A **pipeline** is a DAG of agent jobs defined in YAML. The daemon runs it: jobs with no dependencies start first, and each job's `emit` publishes its output and unblocks its dependents — so a "lead" Claude stays off the critical path. Authoring is CLI-only (`warden pipeline create -f`); the TUI and web show + control pipelines but don't author them.

<!-- media: pipeline DAG diagram (public/media/pipeline-dag.svg) — also embedded on the landing page in Task 7 -->

## Lifecycle

```sh
warden pipeline create -f review.yaml   # validate + register (does NOT start)
warden pipeline start <id>              # spawn jobs with no dependencies
warden pipeline show <id>               # jobs, status, branches, emitted output
warden pipeline list
warden pipeline retry <id> <job>        # re-run a failed/needs-attention job
warden pipeline edit-job <id> <job> --prompt "…"   # edit a still-pending job
warden pipeline cancel <id>             # terminate running jobs
warden pipeline delete <id>             # remove the record (cancel first if live)
```

## Spec

A minimal `analyze → implement → review` chain. **Important:** job prompts must **not** mention `emit` — the daemon auto-appends the emit step and auto-injects each upstream job's output into the dependents' prompts.

```yaml
name: refactor-auth
repo: /Users/me/workspace/app
jobs:
  - id: analyze
    prompt: "Analyze the auth module; no code yet."
    worktree: none
  - id: implement
    prompt: "Implement the refactor described upstream."
    depends_on: [analyze]
    worktree: fresh
    handoff: "the branch name and a 2-line summary"
  - id: review
    prompt: "Merge the implement branch, review, run the suite."
    depends_on: [implement]
    worktree: from:implement
```

Each job's agent finishes by running `warden pipeline emit "<handoff>"`. The pipeline and job IDs are injected into every job's environment automatically (`WARDEN_PIPELINE_ID`, `WARDEN_JOB_ID`), so the agent just runs the command with no flags. Emitting publishes the handoff text to shared context, marks the job `done`, and unblocks any dependents.

Results are durable in the pipeline record (`warden pipeline show`), the shared context (`pipeline.<id>.<job>.output`), and each job's git branch — they are not tied to the (possibly reaped) live agent.

## Worktree strategies (`worktree:` field)

| Value | Behaviour |
|---|---|
| `none` | Agent runs in the repo root; no git worktree created |
| `fresh` | A new git worktree is created on a branch named `<pipeline>-<job>` off HEAD |
| `from:<job>` | A new git worktree is created off the upstream job's branch (for fan-in merges) |

`worktree: from:<job>` bases a job's git worktree on the upstream job's branch. A fan-in job (e.g. `review` above) does the `git merge` itself as part of its prompt work.

## Failure behaviour

If a job's agent session enters `errored` or `orphaned`, the job is marked `failed`, its descendants are marked `skipped`, and the pipeline status becomes `stalled`. Jobs that were already running are not interrupted — only pending descendants are skipped. A `stalled` pipeline can be inspected with `pipeline show` and cleaned up with `pipeline cancel`.

## Pipeline status values

| Status | Meaning |
|---|---|
| `pending` | Created, not yet started |
| `running` | At least one job is in progress |
| `done` | All jobs finished successfully |
| `stalled` | A job failed; its descendants have been skipped |
| `canceled` | Explicitly canceled by the user |

## Editing and recovery

```sh
warden pipeline edit-job <pipeline> <job> --prompt "..." --handoff "..."
warden pipeline retry <pipeline> <job>
```

`edit-job` tweaks a job's prompt and/or handoff *before it starts* (pending jobs only). If a job's agent goes quiet without emitting (its session is flagged `idle` by stuck-detection), the job is marked **`needs_attention`** rather than silently stalling — the pipeline stays `running` and the job is shown flagged. Resolve it by `pipeline emit`-ing on the job's behalf (if the agent actually finished) or `pipeline retry`, which tears down the stale job session/worktree, resets the job, reopens any descendants that were skipped, and re-runs from there.
