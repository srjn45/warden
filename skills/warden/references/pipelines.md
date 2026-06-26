# warden — pipelines (DAG of dependent agent jobs)

A pipeline is a DAG of jobs in one repo. The daemon spawns each job (as a normal
agent) only when its dependencies are `done`, injects upstream outputs into the
job's prompt, and chains git worktrees so code flows downstream. **You author the
YAML spec for the user.**

Pipelines are drivable from **both** the MCP tools and the CLI — prefer MCP when
it's registered. A few admin verbs are CLI-only (noted below).

## MCP tools

| Tool | Purpose |
|---|---|
| `create_pipeline {spec}` | Parse + validate + register a YAML spec; returns `{id, status, jobs}` in `pending`. |
| `start_pipeline {name}` | Spawn the dependency-free entry jobs; the daemon drives the rest. |
| `show_pipeline {name}` | Per-job status + branch + emitted output (durable after agents are gone). |
| `list_pipelines` | All pipelines + status. |
| `cancel_pipeline {name}` | Stop (terminates any live jobs). |

CLI-only admin: `validate`, `list-templates`, `create --template`, `pause`/`resume`,
`edit-job`, `retry`, `delete`, `emit`.

## CLI lifecycle

```sh
warden pipeline validate -f spec.yaml   # client-side spec check (DAG/refs/cycles); exit 0/1, no daemon
warden pipeline create -f spec.yaml     # validate + register  (or: --template <name> [--set K=V])
warden pipeline list-templates          # built-in templates + their placeholders
warden pipeline start <name>            # spawn entry jobs; daemon drives the rest
warden pipeline show <name>             # per-job status + branch + emitted output
warden pipeline list                    # all pipelines + status
warden pipeline pause <name>            # halt new spawns; in-flight jobs finish
warden pipeline resume <name>           # resume spawning
warden pipeline cancel <name>           # stop (terminates live jobs)
warden pipeline delete <name>           # remove the record (cancel first if jobs are live)
```

## Authoring the spec (analyze → implement → review)

```yaml
name: refactor-auth            # also the pipeline id — unique, no '/' or ':'
repo: /abs/path/to/repo
jobs:
  - id: analyze
    prompt: "Analyze the auth module and identify what to change. No code yet."
    worktree: none             # runs in the repo root; touches no code
  - id: implement
    prompt: "Implement the refactor described upstream."
    depends_on: [analyze]
    worktree: fresh            # new branch off repo head; writes code
    handoff: "the branch name and a 2-line summary of what changed"
  - id: review
    prompt: "Merge the implement branch, review the changes, run the test suite."
    depends_on: [implement]
    worktree: from:implement   # branch off implement's branch (builds on its commits)
```

Then create + start (MCP `create_pipeline`/`start_pipeline`, or `warden pipeline
create -f refactor-auth.yaml && warden pipeline start refactor-auth`).

Per-job fields: `id` (required, unique, safe — no `/` `:`), `prompt` (required),
`depends_on` (list of job ids), `worktree` (`none` | `fresh` | `from:<job>`,
default `none`), `handoff` (optional one-line "what to hand downstream"), `run_if`
(`success` | `failure` | `always`, default `success`), `supervised` (optional —
risky tools prompt instead of bypass), `type` (optional, default `development`).

**Worktree modes:** `none` = repo root, read-only/analysis. `fresh` = a new branch
off repo head, for jobs that write code. `from:<job>` = a new branch based on that
upstream job's branch, inheriting its commits; a fan-in job (`depends_on: [a, b]`)
runs `git merge` itself.

**Conditional steps (`run_if`):** a job runs only when its dependencies settled the
right way. `failure`/`always` handlers let a pipeline route around a failed upstream
and still complete; the handler's prompt is told which upstream failed.

**Authoring rule (important):** write each job's `prompt` as a plain task
description plus a `handoff` line. **Do NOT put `warden pipeline emit` instructions
in the prompt** — the daemon auto-appends the emit footer to every job and
auto-injects each upstream job's output into its dependents' prompts. You only
describe the work.

## Templates

Four bundled starters — `analyze-implement-review`, `parallel-tasks`,
`test-fix-verify`, `research-synthesis`. Render with `warden pipeline create
--template <name>`; `{{NAME}}`/`{{REPO}}` auto-fill, other placeholders via
`--set KEY=VALUE`. `warden pipeline list-templates` lists them.

## Driving / recovering

| Intent | Command |
|---|---|
| publish a job's handoff (an agent runs this itself when done; a lead can run it on a job's behalf) | `warden pipeline emit "<text>" [--pipeline <p> --job <j>]` (defaults from `$WARDEN_PIPELINE_ID`/`$WARDEN_JOB_ID`) |
| tweak a *pending* job before it starts | `warden pipeline edit-job <p> <job> --prompt "…" --handoff "…"` |
| re-run a failed / needs-attention job (reopens skipped descendants) | `warden pipeline retry <p> <job>` |

A job whose agent goes quiet without emitting is flagged `needs_attention` (the
pipeline stays `running`) — resolve it with `emit` (if it actually finished) or
`retry`. **Results are durable:** `show_pipeline` / `warden pipeline show` prints
each job's branch and emitted output even after the agents are gone (also in
shared-context keys `pipeline.<id>.<job>.output` and on the job branches).
