---
name: warden
description: Use to manage Claude Code agent sessions via warden — spawn, list, monitor, talk to, and tear down agents; run multi-stage DAG **pipelines** of dependent agent jobs; and share data / pass messages between agents. Triggers include "spawn/create an agent", "list/check/triage my agents", "what is agent <id> doing", "tell/ask agent <id> to …", "terminate/kill agent(s)"; "create/run/show/cancel/delete a pipeline", "run these steps in order", "multi-stage or dependent agent work", an analyze→implement→review chain; "this is a big/multi-phase/long-running task", "this will take a while", "break this down into stages"; "share data between agents", "have one agent message/ask another". Drives the warden MCP tools or the warden CLI — both first-class; pipelines are CLI-only.
---

# warden — drive your agent fleet & pipelines

warden (the CLI is `warden`, aliased as `wd`) runs a local daemon that manages per-task Claude Code agents (each in its
own tmux session, some in a git worktree). You can put them to work three ways:

- **plain agents** — independent one-off tasks;
- **pipelines** — a DAG of agent jobs the daemon spawns lazily as dependencies
  finish, passing each job's output (and git branch) downstream;
- **shared context + messages** — a KV blackboard and per-agent inboxes for ad-hoc
  coordination between agents.

Drive it through the **warden MCP tools** (when registered) or the **`warden`
CLI** (always available — and the only way to drive pipelines). MCP registration may
be blocked by enterprise policy; when the MCP tools are absent, use the CLI — no
capability is lost.

## Choosing the tool (read this first)

**Litmus test: does any step need to wait for another step's result — its output
or its code — before it can start?** No → plain agent(s). Yes → pipeline.

**Second axis — size & longevity:** would one agent accumulate a large or
long-lived context (a multi-phase task, a long unattended run, anything likely to
approach the context limit and auto-compact)? If yes, **decompose it into a
pipeline of bounded stages** — even when the steps are sequential and one agent
could do them. Each stage gets a fresh, small context and is **torn down on
completion**, returning memory to the OS and avoiding the compaction spikes a
long-lived large-context agent causes.

| Use… | When | How |
|---|---|---|
| **Plain agent** *(default)* | one self-contained task; OR several **independent** tasks (none needs another's result — just spawn several) | `spawn_agent` / `warden start "…"` |
| **Pipeline** | **dependent stages** — sequential handoff (analyze→implement→review), fan-out→fan-in (parallel work → a synthesis/merge step), code flowing downstream (a later job builds on an earlier job's branch), or anything to run **unattended** | `warden pipeline create -f spec.yaml` then `start` |
| **ctx / msg** | ad-hoc coordination between otherwise-independent agents — a shared scratchpad, or one agent asking another a question | `warden ctx …` / `warden msg …` |

**Don't:** use a pipeline for a single task (needless overhead — use a plain agent);
use plain agents + manual relay for a clear dependency chain (that's exactly what a
pipeline automates); hand-roll `ctx`/`msg` coordination that a pipeline already
gives you; run a big multi-phase task as one long-lived plain agent — decompose it
into pipeline stages so each agent stays small and closes when its phase finishes.

## Preconditions

- The daemon must be running. If a tool/command returns a connection / "daemon not
  running" error, tell the user to start it (`warden daemon`, or via launchd) — do
  not guess at state.
- MCP tools (when registered) and the `warden` CLI both wrap the same daemon REST
  API. Pipelines are **CLI-only** (no MCP tools) — drive them with `warden
  pipeline …`.

---

# 1) Plain agents (the default)

Manage one-off agents through the MCP tools (`list_agents`, `get_agent`,
`spawn_agent`, `send_to_agent`, `get_agent_output`, `terminate_agent`,
`delete_agent`, `remove_worktree`, `restore_agent`, `adopt_agent`) or the CLI.

The per-agent tools take a `ticket` argument — the agent's **id** as shown by
`list_agents` (for prompt-spawned agents it looks like `agent-<shortid>`).

## Intent → action

| The user wants to… | Do this |
|---|---|
| list / check / triage agents | `list_agents`; summarize by status. Call out `waiting_for_input` (needs them) and `errored`/`orphaned`. Show each agent's `subject` and `workdir`. |
| spin up / create an agent to do X | `spawn_agent` with `prompt: "X"` (auto-typed, no repo needed). Only use `type`+`repo` (+`branch`/`pr`/`worktree`) when the user explicitly wants a managed worktree tied to a repo/ticket. |
| what is agent <id> doing / its status | `get_agent` (status, subject, workdir, events) + `get_agent_output` (recent terminal) → report concisely. |
| tell / ask agent <id> to do Y | `send_to_agent` (the id as `ticket`, plus `text`). Echo back what you sent. |
| stop / terminate / kill <id> | `terminate_agent` (id) — kills tmux+claude, keeps record+worktree; reversible via `restore_agent` |
| clear / delete an agent's record | `delete_agent` (id, hard?) — archives by default |
| remove an agent's worktree | `remove_worktree` (id, force?) — DESTRUCTIVE; **confirm with the user first**; terminate the agent first |
| restore / bring back a lost or orphaned agent | `restore_agent` (id) — only for sessions whose tmux is gone (status `orphaned`); resumes the same conversation |
| adopt / register an existing Claude session | `adopt_agent` (dir?, session_id?, tmux_session?) |

## CLI command map (when not using MCP tools)

| Intent | CLI command |
|---|---|
| list / triage agents | `warden ls` (add `--json` for machine-readable output) |
| full status of one agent | `warden status <id>` (add `--json`) |
| recent terminal output | `warden tail <id>` |
| spawn from a prompt | `warden start "<prompt>"` |
| spawn a managed worktree agent | `warden start <TICKET> --type <TYPE> --repo <repo>` |
| send a message to an agent | `warden send <id> "<text>"` |
| terminate / clean up | `warden done <id>` (terminate + clear record; keeps the worktree). Remove the worktree with `warden remove-worktree <id>` (guarded; `--force` overrides) |
| restore a lost/orphaned agent | `warden restore <id>` |
| adopt an existing session | `warden adopt [--session-id <uuid>] [--dir <path>]` |
| attach interactively | `warden attach <id>` |
| rotate yourself into a fresh agent (free your context) | `/warden rotate` — see "Rotating a long-running agent" below (self only) |

## Rotating a long-running agent into a fresh one (self-rotation)

When **you yourself** are a long-running agent whose context has grown large and the
user runs `/warden rotate`, hand your work to a fresh successor in the same
workspace, then retire yourself. This bounds context and returns memory to the OS
without losing the task. **Self only** — you rotate the agent you are running in
(your id is in `$WARDEN_SESSION_ID`); there is no remote rotate.

Two phases, with a human review gate between them:

1. **Prepare (you do this directly — you have your own context).**
   - Write a **handoff file** to a unique, per-agent path so concurrent
     rotations never overwrite each other — use the OS temp dir keyed on your
     session id: `${TMPDIR:-/tmp}/warden-rotate-handoff-$WARDEN_SESSION_ID.md`.
     (Temp keeps it self-cleaning; the session id keeps it yours.) Capture what
     a fresh agent needs to *continue*: the goal, current working-tree state
     (branch, committed vs. uncommitted), key decisions and approaches already
     ruled out, precise next steps, and pointers to the relevant files.
   - Compose a one-paragraph **resume prompt** — the successor's initial task.
   - Show the user the handoff file path and the resume prompt, and **stop**. Let
     them edit the file and confirm before you go further.

2. **Commit (only after the user says go):**

   ```sh
   HANDOFF="${TMPDIR:-/tmp}/warden-rotate-handoff-$WARDEN_SESSION_ID.md"
   warden rotate --confirm \
     --resume-file "$HANDOFF" \
     --resume-prompt "<the resume prompt>"
   ```

   This spawns the successor in your exact working directory (same worktree, same
   supervised mode), prints the new agent id, then retires you. The successor
   deletes the temp handoff file once it has read it (and the OS clears `/tmp`
   regardless), so nothing is left behind. Nothing irreversible happens without
   `--confirm`.

Do **not** spawn the successor or terminate yourself by hand — `warden rotate`
inherits your launch config and orders spawn-before-reap safely (a failed spawn
leaves you running, so no work is stranded).

---

# 2) Pipelines (CLI only)

A pipeline is a DAG of jobs in one repo. The daemon spawns each job (as a normal
agent) only when its dependencies are `done`, injects upstream outputs into the
job's prompt, and chains git worktrees so code flows downstream. You author the
YAML spec for the user.

## Lifecycle

```sh
warden pipeline create -f spec.yaml   # validate (DAG/refs/cycles) + register
warden pipeline start <name>          # spawn jobs with no deps; the daemon drives the rest
warden pipeline show <name>           # per-job status + branch + emitted output
warden pipeline list                  # all pipelines + status
warden pipeline cancel <name>         # stop (terminates any live jobs)
warden pipeline delete <name>         # remove the record (cancel first if jobs are live)
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

Then: `warden pipeline create -f refactor-auth.yaml && warden pipeline start refactor-auth`.

Per-job fields: `id` (required, unique, safe — no `/` `:`), `prompt` (required),
`depends_on` (list of job ids), `worktree` (`none` | `fresh` | `from:<job>`,
default `none`), `handoff` (optional one-line "what to hand downstream"),
`supervised` (optional — risky tools prompt instead of bypass), `type` (optional,
default `development`).

**Worktree modes:** `none` = repo root, read-only/analysis. `fresh` = a new branch
off repo head, for jobs that write code. `from:<job>` = a new branch based on that
upstream job's branch, so the job inherits the upstream's commits; a fan-in job
(`depends_on: [a, b]`) runs `git merge` itself.

**Authoring rule (important):** write each job's `prompt` as a plain task
description, plus a `handoff` line for what it should pass downstream. **Do NOT put
`warden pipeline emit` instructions in the prompt** — the daemon auto-appends the
emit footer to every job and auto-injects each upstream job's output into its
dependents' prompts. You only describe the work.

## Driving / recovering

| Intent | Command |
|---|---|
| publish a job's handoff (an agent runs this itself when done; you or a lead can run it on a job's behalf) | `warden pipeline emit "<text>" [--pipeline <p> --job <j>]` (defaults from `$WARDEN_PIPELINE_ID`/`$WARDEN_JOB_ID`) |
| tweak a *pending* job before it starts | `warden pipeline edit-job <p> <job> --prompt "…" --handoff "…"` |
| re-run a failed / needs-attention job (reopens skipped descendants) | `warden pipeline retry <p> <job>` |

A job whose agent goes quiet without emitting is flagged `needs_attention` (the
pipeline stays `running`) — resolve it with `emit` (if it actually finished) or
`retry`. **Results are durable:** `warden pipeline show` prints each job's branch
and emitted output even after the agents are gone (also in shared-context keys
`pipeline.<id>.<job>.output` and on the job branches).

---

# 3) Shared context & messages (ad-hoc coordination)

For independent agents that occasionally need to share data or talk — when you
want light cross-talk but not a full pipeline.

**Shared context** — a namespaced key/value blackboard agents read and write:

```sh
warden ctx set <key> <value>     # or: --file <path> / --stdin
warden ctx get <key>
warden ctx list [<prefix>]
warden ctx del <key>
```

Keys are dot-namespaced (`global.*`, `agent.<id>.*`, `pipeline.<id>.*`). Writes are
attributed to `$WARDEN_SESSION_ID` (set per agent) or `--as <id>`.

**Directed messages** — a durable per-agent inbox:

```sh
warden msg send <agent-id> "<message>"        # delivers; wakes it only if idle/waiting
warden msg inbox [--unread]                   # read my messages (marks read)
warden msg wait [--from <id>] [--timeout <sec>]  # block until a message, then print it
```

A *working* agent is never interrupted (woken only when idle/waiting). `msg wait`
blocks in the daemon, so an agent awaits a reply in a single call with no busy-loop.
Identity defaults to `$WARDEN_SESSION_ID`; override with `--as <id>`.

---

## Guardrails

- **`remove_worktree` is destructive — confirm with the user first** (name the agent
  + that its worktree and branch will be deleted); it refuses while the agent runs or
  has uncommitted/unpushed work unless `force:true`. `terminate_agent` is the safe
  "stop this agent" default (reversible via `restore_agent`). Never bulk
  terminate/delete/remove without explicit confirmation.
- **Cancel a pipeline before deleting it** — `pipeline delete` refuses while any job
  is live (running / needs-attention).
- Never fabricate state — read it (`list_agents`/`get_agent`/`get_agent_output`,
  `warden pipeline show`).
- When the daemon is unreachable, say so plainly and stop — don't invent results.
- Don't hand-roll coordination with `ctx`/`msg` that a pipeline already provides.

## Examples

- "Investigate the flaky auth test." → one self-contained task →
  `spawn_agent {prompt: "investigate the flaky auth test and propose a fix"}`.
- "Refactor the auth module, then have it reviewed." → dependent stages → author an
  analyze→implement→review pipeline spec, `pipeline create -f` + `start`, then report
  progress with `pipeline show`.
- "Spin up three agents to each research a different option." → three *independent*
  tasks → three plain `spawn_agent` calls (not a pipeline).
- "What's agent-4f2a up to?" → `get_agent` + `get_agent_output` → report concisely.
