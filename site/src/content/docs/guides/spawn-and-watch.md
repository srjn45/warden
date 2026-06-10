---
title: Spawn & watch agents
description: The two ways to spawn an agent and the commands to observe and interact with one.
---

## Two ways to spawn

### Prompt mode (default — no worktree, auto-typed)

Just pass a quoted prompt. The agent launches in your current directory (or the `--dir` you pass) and **assumes no worktree** — it operates directly on whatever is in that directory. Use `--dir` to point it elsewhere.

```sh
warden start "investigate why the nightly build is flaky"
warden start "summarize the changes in /path/to/repo since last Friday"
```

- **Type is auto-assigned** shortly after spawn (the daemon asks `claude -p` to classify the prompt; falls back to `other` if `claude` isn't available).
- **Subject is auto-generated** — a ≤8-word phrase summarizing current work, seeded from the prompt and refreshed by the poller.

In the web GUI, **+ New agent** opens a single prompt textarea — no type or repo fields.

### Managed worktree mode (`--type`)

When the work belongs to a real repo — especially a development branch tied to a ticket — pass `--type`. This is what creates and manages a git worktree.

```sh
# Development branch for a Jira ticket (new worktree + new branch):
warden start PROJ-350 --type development

# PR review (fresh worktree, runs `gh pr checkout`):
warden start --type pr-review --pr 1234

# Spike/analysis with an opt-in scratch worktree:
warden start --type spike --worktree

# Debug CI — no worktree, runs in the current repo:
warden start --type debug-ci

# Be explicit about repo and branch:
warden start PROJ-350 --type development --repo /path/to/repo --branch my-branch
```

## Observation & interaction

| Command / feature | Description |
|---|---|
| `ls` | List active agents (type, status, age, dir, subject). `--json` for machine output. |
| `status <id>` | Full detail for one agent — workdir, subject, worktree, branch, PR, events. `--json` available. |
| `attach <id>` | Attach your terminal to the agent's tmux session interactively. |
| `send <id> <msg>` | Type a message into the agent's claude session and press Enter. |
| `tail <id>` | Print recent terminal output (`--lines N`). |
| `digest <id>` | Completion digest — files touched, branch, turn count, and a best-effort `claude -p` narrative. `--json` available. |
| **Stuck / attention detection** | Agents flagged `waiting_for_input`, `idle` (stuck), `orphaned`, or `errored`, surfaced across all interfaces. |

`warden ls` shows `ID  TYPE  STATUS  AGE  DIR  SUBJECT`. `DIR` is the base name of the working directory; `SUBJECT` is empty until the first poller refresh; `TYPE` shows `…` while a prompt agent is still being classified. Use `--json` for machine-readable output (a JSON array of full session objects; an empty fleet prints `[]`).

```sh
warden ls
warden ls --json
warden status agent-a1b2          # full detail + event timeline
warden tail agent-a1b2 --lines 80
warden send agent-a1b2 "run the unit tests and fix any failures"
warden attach agent-a1b2          # Ctrl-b d to detach
```
