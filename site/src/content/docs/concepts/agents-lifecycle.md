---
title: Agents & lifecycle
description: How agents are spawned and auto-classified, and the commands that stop, restore, and clean them up.
---

## Spawning agents

| Feature | Description |
|---|---|
| **Prompt-spawn** | `warden start "<prompt>"` — no repo or type needed. Runs `claude` in the caller's directory (or `--dir`). |
| **Auto-classification** | The daemon classifies a prompt-spawned agent's type with `claude -p` shortly after creation (falls back to `other`). |
| **Auto-generated subject** | Each agent gets a one-line ≤8-word summary of what it's doing, seeded from the prompt and refreshed by the poller from the transcript or tmux pane (throttled, change-gated). |
| **Managed worktree spawn** | `--type` creates/adopts a git worktree where the type needs one. |
| **Worktree adoption** | If a worktree for the ticket already exists, the spawn reattaches to it instead of erroring. |
| **Supervised mode** | `--supervised` launches with `--permission-mode acceptEdits` (risky tools prompt → feed the approvals inbox) instead of the default `--dangerously-skip-permissions` bypass. Persisted and reused on restore. |

## Lifecycle management

| Command / feature | Description |
|---|---|
| `stop` | **The single umbrella teardown verb.** Default = full teardown: terminate + clear (archive) record + remove worktree (asks first unless `--yes`). Subtractive flags: `--keep-record`, `--keep-worktree` (`--keep-worktree` alone == `done`), `--hard`, `--pr`/`--base`, `--force`, `--delete-adopted-branch`. Safe order: PR → terminate → clear record → remove worktree. |
| `terminate` | Stop an agent (kill tmux + the agent process); **keeps** the record and worktree. The safe, reversible "stop" default. Alias for `stop --keep-record --keep-worktree`. |
| `restore` | Recreate and resume a lost/orphaned agent's session (`claude --resume`). In Cockpit TUI, select the orphaned agent and press `r`. |
| `recover` | Safety net for the tombstone reaper: revive an **archived** record whose tmux session is confirmed still alive. Bare `recover` only reports candidates; `--apply` re-inserts each one under its original id (children reconnect automatically). |
| `done` | Terminate **and** clear the record in one step (worktree kept). `--hard` purges instead of archiving. Alias for `stop --keep-worktree`. |
| `delete` | Clear the stored record (archive by default, `--hard` purge). Leaves tmux + worktree alone. Alias for `stop --keep-worktree` (record only). |
| `remove-worktree` | Remove the git worktree + branch. **Destructive** — refuses while the agent runs or has uncommitted/unpushed work unless `--force`. Alias for `stop --keep-record` (worktree only). |
| `adopt` | Register an existing Claude session — resume newest-for-dir under tmux, or live-register a running tmux session. |
| **Cascade cleanup** | Deleting a pipeline/agent cascades cleanup of its shared-context keys and (on hard-delete) its mailbox inbox. |

## Status values you'll see

| Status | Meaning |
|---|---|
| `spawning` | Session is being created |
| `working` | Actively doing work |
| `waiting_for_input` | Paused on a question/notification — `send` it an answer |
| `idle` | Alive but not currently working |
| `rate_limited` | Hit a session, weekly, or monthly-spend limit — warden auto-picks "Stop and wait", parks the agent, and auto-resumes when the limit clears (tune via `rate_limit.*`) |
| `done` | Finished |
| `errored` | Hit an error |
| `orphaned` | The daemon lost track of its tmux session |
