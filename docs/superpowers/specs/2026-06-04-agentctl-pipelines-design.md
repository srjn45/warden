# agentctl Pipelines & Inter-Agent Communication — Design

**Date:** 2026-06-04
**Status:** Approved design, pre-implementation

## 1. Summary

Today agentctl agents are islands: each is a Claude Code process in its own tmux
session, and the only communication is hub-and-spoke — a human (or a lead Claude)
uses `send_to_agent` to push text into a session and reads output back. Agents
never address each other, and nothing flows automatically from one agent's
completion to another's start.

This feature adds two layers:

- **Layer 0 — the communication substrate** (the *major prerequisite*):
  - **Shared context** — a daemon-owned, namespaced key/value store agents read and write.
  - **Directed messages** — agent-addressed messages with an inbox, a wake mechanism, and a cheap blocking wait.
- **Layer 1 — pipelines**: a DAG of agent *jobs* the daemon lazily spawns as
  dependencies complete, with outputs flowing downstream automatically. Pipelines
  are built entirely on Layer 0.

The design keeps the **daemon dumb and reliable** and the **intelligence in the
agents**. The daemon routes messages, stores shared context, watches statuses,
and spawns jobs; agents do all reasoning, coding, and git work.

## 2. Goals & non-goals

### Goals
- Let agents pass data to each other (shared context) and talk to each other (directed messages) without a human relaying.
- Let a lead agent (or human) declare a DAG of jobs that the daemon executes unattended.
- Lazy spawn: a job becomes a live session only when its dependencies are done.
- Make all of this reachable from the **agentctl CLI** (the guaranteed path, since MCP is restricted in some environments). MCP and HTTP mirror the CLI where available.
- Surface pipelines in the existing TUI cockpit and web mission-control without a parallel UI.

### Non-goals (deliberately cut from v1)
- No conditional or looping edges — pure DAG only.
- No daemon-side git merging — **agents run `git merge` and resolve conflicts themselves**; the daemon only bases worktrees on branches.
- No dynamic fan-out (`spawn N copies`) — jobs are declared explicitly.
- No cross-pipeline dependencies, no scheduling/cron of pipelines.
- No request routing intelligence — the daemon does not interpret message contents.

## 3. Architecture: two layers

```
┌─────────────────────────────────────────────────────────────┐
│ Layer 1: Pipelines (DAG executor)                            │
│   - job specs, lazy spawn on deps-done, output injection     │
│   - built ENTIRELY on Layer 0 primitives                     │
├─────────────────────────────────────────────────────────────┤
│ Layer 0: Communication substrate                             │
│   (a) Shared context  — namespaced KV, durable               │
│   (b) Directed messages — inbox + wake + blocking wait       │
├─────────────────────────────────────────────────────────────┤
│ Existing: sessions, lifecycle (spawn/tmux), poller, store,   │
│           notify, approvals, TUI cockpit, web mission-control│
└─────────────────────────────────────────────────────────────┘
```

Pure logic lives in new packages mirroring the `internal/approval` convention:
`internal/ctxstore` (shared context), `internal/mailbox` (directed messages),
`internal/pipeline` (DAG logic). The daemon wires them to the file store and
lifecycle.

## 4. Agent self-identification

For any of these CLI commands to be ergonomic from inside an agent, the agent
must know "who am I." At spawn time the daemon sets environment variables on the
tmux session (propagated to the Claude process and its `Bash` tool subprocesses):

- `AGENTCTL_SESSION_ID` — the agent's own session id (all agents).
- `AGENTCTL_PIPELINE_ID`, `AGENTCTL_JOB_ID` — set only for pipeline jobs.

So inside an agent, `agentctl msg send <to> "..."` knows the sender, and
`agentctl pipeline emit "..."` can default the pipeline/job from the environment.
Every command still accepts explicit `--as <id>` / positional ids so a human or
lead agent can act on any agent's behalf.

**Implementation note:** env is set when the session's tmux is created
(`tmux new-session` in `newAgentSession`), so it reaches the agent's shell tools.

---

# Layer 0 — Communication substrate

## 5. Shared context (the blackboard)

A daemon-owned, durable key/value store. This is how agents publish results and
read each other's work without parsing tmux panes.

### 5.1 Model & scoping

```
ContextEntry
  Key        string   (namespaced — see below)
  Value      string   (text; large values stored as files)
  UpdatedBy  string   (session id of the writer, or "human"/"daemon")
  UpdatedAt  time.Time
```

Keys are namespaced by convention to avoid collisions and to scope reads:

- `global.<name>`          — shared across everything
- `pipeline.<pid>.<name>`  — scoped to one pipeline (job outputs live here)
- `agent.<sid>.<name>`     — an agent's own scratch (rarely cross-read)

Persistence: one JSON file per namespace under `context/` in the file store
(e.g. `context/pipeline.<pid>.json`), so a pipeline's whole context is one file
and cleaning up a pipeline removes its context atomically.

### 5.2 CLI

```
agentctl ctx set <key> <value>           # value inline …
agentctl ctx set <key> --file <path>     # … or from a file (large blobs)
agentctl ctx set <key> --stdin           # … or from stdin
agentctl ctx get <key>                    # prints value (exit 1 if absent)
agentctl ctx list [<prefix>]              # keys (+ writer + ts) under a prefix
agentctl ctx del <key>
```

### 5.3 HTTP / MCP

Daemon endpoints mirror the CLI: `GET/PUT/DELETE /context/{key}`,
`GET /context?prefix=`. MCP tools `ctx_get` / `ctx_set` / `ctx_list` mirror them
where MCP is permitted. The CLI is the guaranteed contract.

### 5.4 Concurrency

Writes are last-write-wins per key, serialized by the daemon (it owns the file).
No transactions, no compare-and-swap in v1 — agents coordinate by using distinct
keys (e.g. each fan-out job writes its own `pipeline.<pid>.<job>.output`), which
is the natural pattern and avoids contention entirely.

## 6. Directed messages (the mailbox)

Agent-to-agent addressed messages. This is what enables peer consultation
("agent B asks agent A a question mid-task") and explicit handoff signals, in a
way that does not require a human relay and does not make agents busy-poll.

### 6.1 Model

```
Message
  ID      string
  From    string   (sender session id, or "human"/"daemon")
  To      string   (recipient session id)
  Body    string
  TS      time.Time
  Read    bool
```

Each agent has an inbox — an ordered list of messages. Persisted per recipient
(`inbox/<sid>.json`), durable across daemon restarts.

### 6.2 Delivery: store always, wake only when safe

Sending a message **always** appends it to the recipient's durable inbox. Whether
the recipient is *woken* depends on its status — and this nuance matters:

- Recipient is **idle / waiting_for_input** (parked) → the daemon injects a
  one-line wake notice via the existing `send_to_agent` mechanism:
  *"📨 New message from `<from>`. Run `agentctl msg inbox` to read."*
- Recipient is **actively working** → **do NOT inject.** Interrupting a working
  agent mid-turn can corrupt its current reasoning/tool call. The message sits in
  the inbox; the recipient sees it when it next checks inbox or calls `msg wait`.

This "never interrupt a working agent" rule is a deliberate safety property.

### 6.3 The cheap blocking wait (key primitive)

A Claude agent cannot sleep/poll cheaply — every check is a full LLM turn. The
solution: **push the waiting into the CLI/daemon, not the LLM loop.**

```
agentctl msg wait [--from <id>] [--timeout <sec>]
```

This command **blocks** (the daemon long-polls / holds the request open) until a
matching message arrives or the timeout fires, then prints it and returns. From
the agent's perspective this is **one `Bash` tool call that happens to take a
while** — zero extra LLM turns, no busy-loop, no risk of the poller marking it
stuck (the daemon knows it's intentionally blocked on a wait).

This is how request/reply works: A sends a question to B, then `msg wait --from B`;
B (woken or on its own check) reads the inbox, does the work, and replies with
`msg send A "..."`; A's blocked call returns the answer.

### 6.4 CLI

```
agentctl msg send <to> <body>            # append to <to>'s inbox; wake if parked
agentctl msg inbox [--unread]            # this agent's messages (marks read)
agentctl msg wait [--from <id>] [--timeout <sec>]   # block until a message
```

Sender/recipient identity defaults from `AGENTCTL_SESSION_ID`; `--as <id>`
overrides for humans/lead acting on behalf of an agent.

### 6.5 HTTP / MCP

`POST /sessions/{id}/messages` (send), `GET /sessions/{id}/messages` (inbox),
and a long-poll `GET /sessions/{id}/messages/wait`. MCP mirrors where permitted.

---

# Layer 1 — Pipelines

## 7. Data model

A new entity alongside `Session`, persisted as `pipelines/<id>.json`.

```
Pipeline
  ID         short id
  Name       user label
  Repo       repo root all jobs operate in
  Status     pending | running | done | failed | stalled
  Jobs       []Job
  CreatedAt, UpdatedAt

Job
  ID          job key, unique in pipeline (e.g. "analyze", "implement")
  Prompt      the input (natural language)
  DependsOn   []JobID
  Handoff     optional one-liner shaping the emit (blank = auto-summary)
  Worktree    none | fresh | from:<jobID>   (cwd strategy at spawn)
  Supervised  bool                          (reuses existing supervised flag)
  Type        store.Type (defaults to development)

  -- filled at runtime --
  SessionID   spawned agent session
  Status      pending | running | done | failed | skipped
  Output      captured emit text (lives in shared context too)
  Branch      branch this job worked on (auto-captured, fed downstream)
  StartedAt, DoneAt
```

`Worktree`:
- `none`        — run in the repo root (analysis jobs that touch no code).
- `fresh`       — new worktree + branch off repo head.
- `from:<jobID>`— new worktree + branch based on that upstream job's branch
  (the common dev-chain case: implement → review off the implementer's branch).

**Back-reference on sessions:** add `PipelineID` and `JobID` to `store.Session`
so the TUI/web can group a pipeline's live agents under the pipeline node rather
than the flat agent list.

## 8. The emit protocol (job handoff)

When the daemon spawns a job, it composes the prompt in three parts:

```
[upstream context block]   ← outputs of all DependsOn jobs, auto-injected
[the job's own prompt]
[pipeline footer]          ← auto-appended protocol instructions
```

**Footer** (auto-appended):
> You are job `<id>` in pipeline `<pid>`. When your task is complete, publish
> your handoff for downstream jobs by running:
> `agentctl pipeline emit "<your handoff text>"`
> [if `Handoff` set:] Include specifically: `<handoff>`

**Upstream context block** (for jobs with deps):
> ### Upstream output — job `analyze`:
> `<analyze.Output>`
> (branch: `branch-analyze`)

`emit` is **sugar over shared context + a status flip**. It does three things at once:
1. **Communication** — writes the text to `pipeline.<pid>.<job>.output` in shared context.
2. **Completion signal** — marks the job `done` (far more reliable than guessing `done` from a quiet pane).
3. **Data flow** — triggers the executor to inject this output into dependents when they spawn.

The daemon **auto-attaches the job's git branch** (read from the session record)
to the emit, so the agent only writes prose; branch names propagate automatically.

**Fallback:** if the poller reports a job session `done`/exited but no `emit`
arrived within a grace window, the daemon flags the job `needs-attention` (does
**not** silently auto-advance). A human or the lead can then `emit` on its behalf
or `retry`.

## 9. Executor

A reconcile loop driven by the **existing `poller.OnTransition` hook** plus each
`emit` call — no new polling infrastructure:

```
reconcile(pipeline):
  sync each job's status from its session + emit state
  if any job failed/errored:
      mark its descendants "skipped"; pipeline -> stalled; notify (internal/notify)
  for each PENDING job whose DependsOn are all DONE:
      spawn:
        - resolve worktree (none | fresh | off upstream branch)
        - compose prompt (upstream outputs + job prompt + footer)
        - set AGENTCTL_* env, then lifecycle.Spawn(...)   # reuses existing path
        - job -> running
  if all jobs done: pipeline -> done
```

The daemon never reads transcripts to make decisions; `emit` is the only
completion signal (with the grace-window fallback above).

## 10. Worktree chaining (code handoff)

Most handoff is text. Code handoff is *also* mostly text — the agents do the git.

- **Linear (A→B, same code):** `Worktree: from:A`. B opens a worktree branched
  off A's branch and keeps committing. Daemon does one `git worktree add -b
  branch-B <path> branch-A`. No merge logic.
- **Fan-out (A→{B,C}):** B and C each get their own worktree branched off A's
  branch, run in parallel, never share a directory.
- **Fan-in (B,C→D):** D's prompt carries B's and C's outputs *including their
  branch names*. **D — a Claude agent — runs `git merge branch-B branch-C`,
  resolves conflicts, runs the suite.** The daemon never merges. This is the
  single hard corner, and it is delegated to the agent, where it belongs.

## 11. CLI surface (consolidated)

Agent-facing (used inside jobs; identity from env):
```
agentctl pipeline emit "<handoff text>"          # defaults pid/job from env
agentctl ctx set/get/list/del ...                # shared context
agentctl msg send/inbox/wait ...                 # directed messages
```

Authoring / control (lead agent or human):
```
agentctl pipeline create -f spec.yaml            # author a DAG (YAML)
agentctl pipeline list
agentctl pipeline show <pid>                      # DAG + per-job status + outputs
agentctl pipeline start <pid>                     # explicit start (no auto-run)
agentctl pipeline edit-job <pid> <job> --prompt … --handoff …   # only while pending
agentctl pipeline retry <pid> <job>               # re-spawn a failed job
agentctl pipeline cancel <pid>
```

### 11.1 Spec format (YAML)

YAML chosen over JSON: a lead agent writes it more naturally and it allows
comments. Example:

```yaml
name: refactor-auth
repo: /Users/me/workspace/app
jobs:
  - id: analyze
    prompt: "Analyze the auth module and identify what to change. No code yet."
    worktree: none

  - id: implement
    prompt: "Implement the refactor described upstream."
    depends_on: [analyze]
    worktree: fresh
    handoff: "the branch name and a 2-line summary of what changed"

  - id: tests
    prompt: "Write tests for the refactor described upstream."
    depends_on: [analyze]
    worktree: fresh
    handoff: "the branch name and which tests you added"

  - id: review
    prompt: "Merge the implement and tests branches, review, run the full suite."
    depends_on: [implement, tests]
    worktree: from:implement
```

Validation at `create`: reject cycles, unknown `depends_on` ids, unknown
`from:<job>` worktree refs — with clear errors.

## 12. TUI integration (fits the existing cockpit)

The cockpit is three tmux panes — top-left list (the Bubble Tea app), bottom-left
master claude (the lead), right detail (attaches to the selected agent or renders
an inline view). The list pane's `items()` is already a tagged union (pinned
`approvals` row, dir-group headers, session rows). Pipelines slot into that exact
pattern — **no new screen, no new mode, no 4th pane:**

1. **New `item` variants** `pipeline` and `pipelineJob` alongside
   `session`/`approvals`/dir-header. Render a grouped **▸ Pipelines** section:
   ```
   ▸ refactor-auth         running
       ● analyze    done
       ◐ implement  running   (depends: analyze)
       ◐ tests      running   (depends: analyze)
       ○ review     pending   (depends: implement, tests)
   ```
2. **Selecting a pipeline renders the DAG inline** in the detail region, reusing
   the path the approvals queue uses (`renderApprovalsQueue` → new `renderPipeline`).
3. **Selecting a running job drives the right pane for free** — a job *is* a
   session (`SessionID`), so attaching reuses the existing per-agent attach. No
   new attach logic.
4. **Pipeline-owned sessions nest under their pipeline** (via `Session.PipelineID/JobID`)
   instead of cluttering the flat dir-grouped list.
5. **Build/edit reuses existing flows:** editing a `pending` job's prompt reuses
   the `modeNewAgent` textarea; adding jobs reuses the new-agent input + a
   dependency checklist (same machinery as the dir-picker).

Keys (consistent with existing): `enter` view job prompt/output, `e` edit a
`pending` job, `n` nudge a running job, `r` retry failed, `x` cancel pipeline,
`a` add job.

## 13. Web integration (mission-control)

A new **Pipelines** tab in the existing tabbed shell:
- Visual DAG (nodes + edges), nodes colored by status, live over the existing
  status/SSE channel.
- Click a node → drawer: prompt (editable textarea if `pending`), handoff hint,
  read-only output, and a link that jumps to that job's existing terminal tab
  (reuses the interactive `tmux attach` terminal).
- "New pipeline" builder: add job cards (prompt + dependency multiselect) → save
  → `pipeline create`. No JSON shown to the user.

## 14. Failure handling

- **Job session errors/orphaned** → job `failed`; descendants `skipped`; pipeline
  `stalled`; notify via `internal/notify`.
- **Done but no emit** → grace window, then `needs-attention`; never silently
  advance. Lead/human can `emit` on its behalf or `retry`.
- **Cycle / unknown dependency at create** → rejected with a clear error.
- **`retry`** re-spawns a failed job fresh; reconcile resumes.
- **Message to a non-existent / terminated recipient** → `msg send` errors clearly.

## 15. Lead agent role (pattern, not code)

The cockpit's bottom-left Claude is the natural lead. It **authors** the spec
(writes `spec.yaml`, runs `pipeline create` + `start`) and **supervises**
(periodic `pipeline show`, nudging a stuck job, or `emit`-ing on behalf of a job
that finished without emitting). Crucially it is **off the critical path** — the
daemon drives the DAG to completion even if the lead is closed or drifts. The
only true single point of failure is the daemon, which is already the foundation
of the whole system; pipelines add no new one.

## 16. Security & safety considerations

- **Never interrupt a working agent** (§6.2) — wake only parked agents.
- Pipeline jobs inherit the existing `Supervised` flag, so risky tools still flow
  through the approvals inbox when supervised.
- Shared context and messages are local to the daemon (same trust boundary as
  sessions today); no new network surface beyond the existing localhost daemon.
- Output injection is plain text prepended to a prompt; it is not executed by the
  daemon — the agent decides what to do with it.

## 17. Testing strategy

- `internal/ctxstore`: set/get/list/del, namespacing, file-per-namespace — table tests.
- `internal/mailbox`: append/read/mark-read, wait-matching (`--from` filter,
  timeout), wake-only-when-parked decision — table tests with a fake clock.
- `internal/pipeline`: ready-set computation, cycle detection, descendant-skip on
  failure, prompt composition (upstream injection + footer) — table tests.
- Executor: reconcile against a fake store + `FakeRunner` lifecycle.
- `emit` endpoint + branch auto-capture; long-poll `msg wait` endpoint.
- Built via the established TDD + subagent-driven worktree workflow.

## 18. Resolved decisions

- **Explicit `start`** (no auto-run on create) — lets you build/review/edit the DAG before anything spawns.
- **YAML** spec format for `pipeline create -f`.

## 19. Suggested implementation phasing

The substrate is a prerequisite, so build bottom-up — each phase is independently
useful and testable:

1. **Shared context** (`internal/ctxstore` + CLI + HTTP/MCP).
2. **Directed messages** (`internal/mailbox` + CLI incl. blocking `wait` + wake rule + HTTP).
3. **Agent self-identification** env vars at spawn.
4. **Pipeline model + executor** (`internal/pipeline`, emit protocol, worktree chaining, CLI, YAML).
5. **TUI integration** (item variants, inline DAG render, session nesting).
6. **Web integration** (Pipelines tab, DAG view, builder).

Phases 1–2 deliver inter-agent communication on their own (peer consultation,
shared blackboard) even before pipelines land.
