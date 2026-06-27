---
title: Pipelines (DAG)
description: Define a DAG of dependent agent jobs in YAML and let the daemon run them — outputs flow downstream automatically.
---

A **pipeline** is a DAG of agent jobs defined in YAML. The daemon runs it: jobs with no dependencies start first, and each job's `emit` publishes its output and unblocks its dependents — so a "lead" Claude stays off the critical path. Author from a YAML spec (`warden pipeline create -f`), from a built-in template (`warden pipeline create --template …`), or over MCP (`create_pipeline`/`start_pipeline`). The TUI and web show + control pipelines but don't author them.

![A pipeline rendered as a DAG in the warden web UI: two parallel root jobs fan out through design and implementation jobs, converge on a three-parent `integrate` job, then fan out again to parallel test/review jobs before a final `release` job — each job card shows its live status.](/warden/media/web-pipeline.png)

Selecting a job opens a drawer with its prompt, emitted output, and completion digest — plus **Cancel** (pipeline) / **Retry** (job) controls and an **Open terminal** link to a running job's session:

![The web pipeline view with a completed job selected: a side drawer shows the job's Prompt, its emitted Output, and the completion Digest.](/warden/media/web-pipeline-job.png)

## Lifecycle

```sh
warden pipeline list-templates          # show the built-in templates + placeholders
warden pipeline validate -f review.yaml # check the spec (DAG/refs/cycles); exit 0/1, no daemon
warden pipeline create -f review.yaml   # validate + register (does NOT start)
warden pipeline start <id>              # spawn jobs with no dependencies
warden pipeline show <id>               # jobs, status, branches, emitted output
warden pipeline list
warden pipeline pause <id>              # let in-flight jobs finish; spawn no new ones
warden pipeline resume <id>             # spawn jobs that became ready while paused
warden pipeline retry <id> <job>        # re-run a failed/needs-attention job
warden pipeline edit-job <id> <job> --prompt "…"   # edit a still-pending job
warden pipeline cancel <id>             # terminate running jobs
warden pipeline delete <id>             # remove the record (cancel first if live)
```

## Built-in templates

Skip writing YAML for common shapes — `warden pipeline create --template <name> --set KEY=value` renders a built-in template (embedded in the binary) and registers it. `warden pipeline list-templates` prints each template and its placeholders.

| Template | Shape | Placeholders |
|---|---|---|
| `analyze-implement-review` | Analyze a task, implement on a fresh branch, then review the result | `NAME`, `REPO`, `TASK` |
| `parallel-tasks` | Run two independent tasks in parallel, then integrate their branches | `NAME`, `REPO`, `TASK_A`, `TASK_B` |
| `test-fix-verify` | Reproduce a failing test, fix the cause, then verify the fix holds | `NAME`, `REPO`, `TASK` |
| `research-synthesis` | Research a topic from two angles in parallel, then synthesize | `NAME`, `REPO`, `TOPIC` |

```sh
warden pipeline create --template analyze-implement-review \
  --set NAME=refactor-auth --set REPO=/path/to/app \
  --set TASK="extract the token-refresh logic into its own module"
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
