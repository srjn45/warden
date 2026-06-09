# agentctl Skill Pipelines-Awareness Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Teach Claude agentctl's full capability surface (pipelines, shared context, messages) and *when to use a plain agent vs a pipeline*, by rewriting the `agentctl` skill and nudging the orchestrator output style.

**Architecture:** Two prose artifacts — a rewritten `skills/agentctl/SKILL.md` (decision rubric + pipelines + ctx/msg, keeping the existing plain-agent content reframed as tier 1) and a ~3-line edit to the user-local `~/.claude/output-styles/orchestrator.md`. No code, no tests; verification is *accuracy* against the live `agentctl` CLI. Spec: `docs/superpowers/specs/2026-06-05-agentctl-skill-pipelines-awareness-design.md`.

**Tech Stack:** Markdown (Claude Code skill + output style). The CLI surface the content must match was captured from `agentctl {pipeline,ctx,msg} --help`.

---

## File Structure

- **Modify (full rewrite)** `skills/agentctl/SKILL.md` — the single agentctl skill. Sections: what-it-is → choosing-the-tool (rubric) → plain agents (existing content) → pipelines → ctx/msg → guardrails. Installed via the existing `make install-skill` symlink (`~/.claude/skills/agentctl` → repo), so no reinstall is needed for the skill to take effect.
- **Modify** `~/.claude/output-styles/orchestrator.md` — step 2 ("Delegate") gains a pipeline branch. User-local (not in the repo); edited in place.

---

## Task 1: Rewrite `skills/agentctl/SKILL.md`

**Files:**
- Modify (replace entire contents): `skills/agentctl/SKILL.md`

- [ ] **Step 1: Replace the file with the new content**

Replace the entire contents of `skills/agentctl/SKILL.md` with exactly this:

````markdown
---
name: agentctl
description: Use to manage Claude Code agent sessions via agentctl — spawn, list, monitor, talk to, and tear down agents; run multi-stage DAG **pipelines** of dependent agent jobs; and share data / pass messages between agents. Triggers include "spawn/create an agent", "list/check/triage my agents", "what is agent <id> doing", "tell/ask agent <id> to …", "terminate/kill agent(s)"; "create/run/show/cancel/delete a pipeline", "run these steps in order", "multi-stage or dependent agent work", an analyze→implement→review chain; "share data between agents", "have one agent message/ask another". Drives the agentctl MCP tools or the agentctl CLI — both first-class; pipelines are CLI-only.
---

# agentctl — drive your agent fleet & pipelines

agentctl runs a local daemon that manages per-task Claude Code agents (each in its
own tmux session, some in a git worktree). You can put them to work three ways:

- **plain agents** — independent one-off tasks;
- **pipelines** — a DAG of agent jobs the daemon spawns lazily as dependencies
  finish, passing each job's output (and git branch) downstream;
- **shared context + messages** — a KV blackboard and per-agent inboxes for ad-hoc
  coordination between agents.

Drive it through the **agentctl MCP tools** (when registered) or the **`agentctl`
CLI** (always available — and the only way to drive pipelines). MCP registration may
be blocked by enterprise policy; when the MCP tools are absent, use the CLI — no
capability is lost.

## Choosing the tool (read this first)

**Litmus test: does any step need to wait for another step's result — its output
or its code — before it can start?** No → plain agent(s). Yes → pipeline.

| Use… | When | How |
|---|---|---|
| **Plain agent** *(default)* | one self-contained task; OR several **independent** tasks (none needs another's result — just spawn several) | `spawn_agent` / `agentctl start "…"` |
| **Pipeline** | **dependent stages** — sequential handoff (analyze→implement→review), fan-out→fan-in (parallel work → a synthesis/merge step), code flowing downstream (a later job builds on an earlier job's branch), or anything to run **unattended** | `agentctl pipeline create -f spec.yaml` then `start` |
| **ctx / msg** | ad-hoc coordination between otherwise-independent agents — a shared scratchpad, or one agent asking another a question | `agentctl ctx …` / `agentctl msg …` |

**Don't:** use a pipeline for a single task (needless overhead — use a plain agent);
use plain agents + manual relay for a clear dependency chain (that's exactly what a
pipeline automates); hand-roll `ctx`/`msg` coordination that a pipeline already
gives you.

## Preconditions

- The daemon must be running. If a tool/command returns a connection / "daemon not
  running" error, tell the user to start it (`agentctl daemon`, or via launchd) — do
  not guess at state.
- MCP tools (when registered) and the `agentctl` CLI both wrap the same daemon REST
  API. Pipelines are **CLI-only** (no MCP tools) — drive them with `agentctl
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
| list / triage agents | `agentctl ls` (add `--json` for machine-readable output) |
| full status of one agent | `agentctl status <id>` (add `--json`) |
| recent terminal output | `agentctl tail <id>` |
| spawn from a prompt | `agentctl start "<prompt>"` |
| spawn a managed worktree agent | `agentctl start <TICKET> --type <TYPE> --repo <repo>` |
| send a message to an agent | `agentctl send <id> "<text>"` |
| terminate / clean up | `agentctl done <id>` (terminate + clear record; keeps the worktree). Remove the worktree with `agentctl remove-worktree <id>` (guarded; `--force` overrides) |
| restore a lost/orphaned agent | `agentctl restore <id>` |
| adopt an existing session | `agentctl adopt [--session-id <uuid>] [--dir <path>]` |
| attach interactively | `agentctl attach <id>` |

---

# 2) Pipelines (CLI only)

A pipeline is a DAG of jobs in one repo. The daemon spawns each job (as a normal
agent) only when its dependencies are `done`, injects upstream outputs into the
job's prompt, and chains git worktrees so code flows downstream. You author the
YAML spec for the user.

## Lifecycle

```sh
agentctl pipeline create -f spec.yaml   # validate (DAG/refs/cycles) + register
agentctl pipeline start <name>          # spawn jobs with no deps; the daemon drives the rest
agentctl pipeline show <name>           # per-job status + branch + emitted output
agentctl pipeline list                  # all pipelines + status
agentctl pipeline cancel <name>         # stop (terminates any live jobs)
agentctl pipeline delete <name>         # remove the record (cancel first if jobs are live)
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

Then: `agentctl pipeline create -f refactor-auth.yaml && agentctl pipeline start refactor-auth`.

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
`agentctl pipeline emit` instructions in the prompt** — the daemon auto-appends the
emit footer to every job and auto-injects each upstream job's output into its
dependents' prompts. You only describe the work.

## Driving / recovering

| Intent | Command |
|---|---|
| publish a job's handoff (an agent runs this itself when done; you or a lead can run it on a job's behalf) | `agentctl pipeline emit "<text>" [--pipeline <p> --job <j>]` (defaults from `$AGENTCTL_PIPELINE_ID`/`$AGENTCTL_JOB_ID`) |
| tweak a *pending* job before it starts | `agentctl pipeline edit-job <p> <job> --prompt "…" --handoff "…"` |
| re-run a failed / needs-attention job (reopens skipped descendants) | `agentctl pipeline retry <p> <job>` |

A job whose agent goes quiet without emitting is flagged `needs_attention` (the
pipeline stays `running`) — resolve it with `emit` (if it actually finished) or
`retry`. **Results are durable:** `agentctl pipeline show` prints each job's branch
and emitted output even after the agents are gone (also in shared-context keys
`pipeline.<id>.<job>.output` and on the job branches).

---

# 3) Shared context & messages (ad-hoc coordination)

For independent agents that occasionally need to share data or talk — when you
want light cross-talk but not a full pipeline.

**Shared context** — a namespaced key/value blackboard agents read and write:

```sh
agentctl ctx set <key> <value>     # or: --file <path> / --stdin
agentctl ctx get <key>
agentctl ctx list [<prefix>]
agentctl ctx del <key>
```

Keys are dot-namespaced (`global.*`, `agent.<id>.*`, `pipeline.<id>.*`). Writes are
attributed to `$AGENTCTL_SESSION_ID` (set per agent) or `--as <id>`.

**Directed messages** — a durable per-agent inbox:

```sh
agentctl msg send <agent-id> "<message>"        # delivers; wakes it only if idle/waiting
agentctl msg inbox [--unread]                   # read my messages (marks read)
agentctl msg wait [--from <id>] [--timeout <sec>]  # block until a message, then print it
```

A *working* agent is never interrupted (woken only when idle/waiting). `msg wait`
blocks in the daemon, so an agent awaits a reply in a single call with no busy-loop.
Identity defaults to `$AGENTCTL_SESSION_ID`; override with `--as <id>`.

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
  `agentctl pipeline show`).
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
````

- [ ] **Step 2: Verify the content against the live CLI**

Run each of these and confirm every command/flag/field used in the skill matches:

Run: `agentctl pipeline --help` → has `create/list/show/start/cancel/delete/emit/edit-job/retry`.
Run: `agentctl ctx --help` → has `set/get/list/del`.
Run: `agentctl msg --help` → has `send/inbox/wait`.
Run: `agentctl pipeline emit --help` → has `--pipeline`/`--job`.
Run: `agentctl msg wait --help` → has `--from`/`--timeout`/`--as`.

Expected: every verb/flag named in the skill exists. (If any drifted, fix the skill text to match.)

- [ ] **Step 3: Verify frontmatter + structure**

Run: `head -4 skills/agentctl/SKILL.md`
Expected: a single `---`-delimited frontmatter block with `name: agentctl` and a one-line `description:` that mentions pipelines + ctx/msg triggers.

Run: `grep -c "^# \|^## " skills/agentctl/SKILL.md`
Expected: the section headers are present (Choosing the tool, the three numbered sections, Guardrails).

- [ ] **Step 4: Commit**

```bash
git add skills/agentctl/SKILL.md
git commit -m "docs(skill): teach agentctl pipelines/ctx/msg + plain-vs-pipeline rubric"
```

---

## Task 2: Nudge the orchestrator output style

**Files:**
- Modify: `~/.claude/output-styles/orchestrator.md` (user-local; not in the repo)

- [ ] **Step 1: Read the current step 2**

Run: `sed -n '/## Loop for every incoming request/,/3\. \*\*Monitor/p' ~/.claude/output-styles/orchestrator.md`
Expected: shows the current step 2 ("**Delegate.** If nothing fits, spawn a new agent…").

- [ ] **Step 2: Replace step 2 with the pipeline-aware version**

In `~/.claude/output-styles/orchestrator.md`, replace the line that currently reads (approximately):

```
2. **Delegate.** If nothing fits, spawn a new agent with a clear, self-contained prompt (`spawn_agent` with `prompt: "…"`, or `agentctl start "<prompt>"`). Let agentctl auto-classify the type.
```

with:

```
2. **Delegate.** If nothing fits: for a self-contained task, spawn a **plain agent** (`spawn_agent` with `prompt: "…"`, or `agentctl start "<prompt>"`); for work with **dependent stages** — one step needs another's result (e.g. analyze→implement→review, or fan-out then merge) — set up a **pipeline** instead (`agentctl pipeline create -f spec.yaml` then `start`). Use the agentctl skill's rubric to choose. Let agentctl auto-classify plain-agent types.
```

(Match the exact existing wording when locating the line; if it differs slightly, replace the single "**Delegate.**" numbered item with the version above.)

- [ ] **Step 3: Verify it reads cleanly**

Run: `sed -n '/2\. \*\*Delegate/,/3\. \*\*Monitor/p' ~/.claude/output-styles/orchestrator.md`
Expected: the new step 2 mentions both plain agents and pipelines and points at the skill's rubric. (No commit — this file is user-local, outside the repo.)

---

## Task 3: Final accuracy pass

**Files:** none (review only)

- [ ] **Step 1: Cross-check the worked example against a real run path**

Re-read the pipelines section of `skills/agentctl/SKILL.md`. Confirm:
- the YAML field names match the spec (`name`, `repo`, `jobs[: id, prompt, depends_on, worktree, handoff, supervised, type]`);
- `worktree` values are exactly `none` / `fresh` / `from:<job>`;
- the lifecycle commands and the driving/recovery commands all appear in `agentctl pipeline --help`;
- the authoring rule (no `emit` in prompts; daemon auto-appends) is stated.

- [ ] **Step 2: Confirm the skill is the installed one**

Run: `ls -l ~/.claude/skills/agentctl`
Expected: a symlink to the repo's `skills/agentctl` (so the rewrite is already live; `make install-skill` re-links it if not). No daemon reinstall is required — the skill is read by Claude Code at session start, not by the daemon.

- [ ] **Step 3: Done**

No further commits. Report: the skill rewrite is committed; the orchestrator nudge is applied to the user-local file; both are accurate against the live CLI.

---

## Verification checklist (after all tasks)

- [ ] `skills/agentctl/SKILL.md` rewritten + committed; valid frontmatter; triggers cover pipeline/ctx/msg intents.
- [ ] Every command/flag/YAML field in the skill matches `agentctl {,pipeline,ctx,msg} --help`.
- [ ] `~/.claude/output-styles/orchestrator.md` step 2 mentions pipelines and points at the rubric.
- [ ] Manual sanity: reading the rubric, the plain-vs-pipeline choice is obvious for "investigate X" (plain) vs "refactor then review" (pipeline) vs "three independent researches" (three plain agents).

## Non-goals (per spec)

- No MCP pipeline tools (pipelines stay CLI-driven for Claude).
- No Go/daemon changes; no new skill files.
- No repo-tracked copy of the orchestrator output style (stays user-local).
