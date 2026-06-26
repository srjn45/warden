# warden — plain agents (lifecycle, rotate, handoff)

Plain agents are the default: independent one-off tasks. Manage them through the
MCP tools (`list_agents`, `get_agent`, `spawn_agent`, `send_to_agent`,
`get_agent_output`, `terminate_agent`, `delete_agent`, `remove_worktree`,
`restore_agent`, `adopt_agent`) or the CLI. The per-agent tools take a `ticket` —
the agent's **id** from `list_agents` (prompt-spawned ids look like
`agent-<shortid>`).

## Intent → action (MCP)

| The user wants to… | Do this |
|---|---|
| list / check / triage agents | `list_agents`; summarize by status. Call out `waiting_for_input` (needs them) and `errored`/`orphaned`. Show each agent's `subject` and `workdir`. |
| spin up an agent to do X | `spawn_agent {prompt: "X"}` (auto-typed, no repo needed). Only add `type`+`repo` (+`branch`/`pr`/`worktree`) for a managed worktree tied to a repo/ticket. Add `model`, `permission_mode`/`supervised`, `tags` as needed. |
| what is agent <id> doing | `get_agent` (status, subject, workdir, events) + `get_agent_output` (recent terminal) → report concisely. |
| tell / ask agent <id> to do Y | `send_to_agent` (id as `ticket`, plus `text`). Echo back what you sent. |
| stop / terminate / kill <id> | `terminate_agent` — kills tmux+claude, keeps record+worktree; reversible via `restore_agent`. |
| clear / delete an agent's record | `delete_agent` (id, `hard?`) — archives by default. |
| remove an agent's worktree | `remove_worktree` (id, `force?`) — DESTRUCTIVE; **confirm with the user first**; terminate the agent first. |
| restore a lost/orphaned agent | `restore_agent` (id) — only for sessions whose tmux is gone (status `orphaned`); resumes the same conversation. |
| adopt an existing Claude session | `adopt_agent` (dir?, session_id?, tmux_session?). |

## CLI command map

| Intent | CLI command |
|---|---|
| list / triage agents | `warden ls` (`--json`; `--watch`/`-w` live-updates over SSE; `--tag <t>` repeatable, AND semantics) |
| full status of one agent | `warden status <id>` (`--json`) |
| recent terminal output | `warden tail <id>` (`--lines N`) |
| spawn from a prompt | `warden start "<prompt>"` |
| spawn a managed worktree agent | `warden start <TICKET> --type <TYPE> --repo <repo>` |
| send a message to an agent | `warden send <id> "<text>"` |
| terminate + clear record (keeps worktree) | `warden done <id>` (`--create-pr` pushes the branch and opens a GitHub PR before terminating, `--base` sets target, default main) |
| remove the worktree | `warden remove-worktree <id>` (guarded; `--force` overrides) |
| restore a lost/orphaned agent | `warden restore <id>` |
| adopt an existing session | `warden adopt [--session-id <uuid>] [--dir <path>]` |
| attach interactively | `warden attach <id>` |
| completion digest | `warden digest <id>` (`--json`) — files touched, branch, turn count, narrative |
| search the fleet | `warden search <query…>` (terms ANDed; `--closed` folds in archive; `--json`) |
| browse the archive | `warden history` (`--since 24h|7d|2w|date`, `--type`, `--limit`, `--json`) |
| rotate yourself into a fresh agent | `/warden rotate` — see below (self only) |
| delegate a sub-task to another agent | `/warden handoff` — see below (you keep running) |

## Spawn options worth knowing

- **Model** — `--model` (CLI) / `model` (MCP). Aliases `opus`/`sonnet`/`haiku`/`fable`;
  config default `model_default`; fallback `claude-sonnet-4-6`. Shown in the MODEL
  column, preserved on restore.
- **Permission mode** — `--permission-mode <acceptEdits|auto|bypassPermissions|default|dontAsk|plan>`
  (legacy `--supervised` = `acceptEdits`). Global default `default_permission_mode`
  (defaults `auto`). Change at runtime: `warden set-permission-mode <id> <mode>`.
- **Presets** — `warden preset save <name> [spawn flags]` persists
  `--type`/`--model`/`--permission-mode`/`--auto-restart`/`--worktree`/`--in-repo`;
  `warden preset list`; `warden start --preset <name>` (explicit flags still
  override). CLI-only.
- **Tags** — `warden start --tags backend,urgent` (lowercased, deduped). Part of
  the search haystack; filter with `warden ls --tag …`.
- **Task types (`--type`)** — `development`/`code`/`docs`/`website`/`debug-ci`/`tests`
  get a fresh-branch worktree (isolated in `.worktrees/<id>`; `--in-repo` shares the
  repo). `pr-review` checks out a PR branch (needs `--pr`/`--branch`).
  `analysis`/`spike` run in the repo unless `--worktree`. `other` = no worktree.

## Spawn gate

`spawn_gate` (default on, `spawn_gate_max_agents` = 5) warns before spawning when
many agents are already live. `--force` (or MCP `force:true`) spawns anyway. The
same gate governs new-delegate `handoff`.

## Rotating a long-running agent into a fresh one (self-rotation)

When **you yourself** are a long-running agent whose context has grown large and
the user runs `/warden rotate`, hand your work to a fresh successor in the same
workspace, then retire yourself. This bounds context and returns memory to the OS
without losing the task. **Self only** — you rotate the agent you run in (id in
`$WARDEN_SESSION_ID`); there is no remote rotate.

Two phases, with a human review gate between them:

1. **Prepare (you do this directly — you have your own context).**
   - Write a **handoff file** to a unique per-agent path so concurrent rotations
     never collide — `${TMPDIR:-/tmp}/warden-rotate-handoff-$WARDEN_SESSION_ID.md`.
     Capture what a fresh agent needs to *continue*: the goal, current working-tree
     state (branch, committed vs. uncommitted), key decisions and approaches ruled
     out, precise next steps, and pointers to the relevant files.
   - Compose a one-paragraph **resume prompt** — the successor's initial task.
   - Show the user the handoff file path and the resume prompt, and **stop** for
     review before going further.

2. **Commit (only after the user says go):**

   ```sh
   HANDOFF="${TMPDIR:-/tmp}/warden-rotate-handoff-$WARDEN_SESSION_ID.md"
   warden rotate --confirm \
     --resume-file "$HANDOFF" \
     --resume-prompt "<the resume prompt>"
   ```

   This spawns the successor in your exact working directory (same worktree, same
   supervised mode), prints the new agent id, then retires you. The successor
   deletes the temp handoff file once read. Nothing irreversible happens without
   `--confirm`.

Do **not** spawn the successor or terminate yourself by hand — `warden rotate`
inherits your launch config and orders spawn-before-reap safely (a failed spawn
leaves you running, so no work is stranded).

## Delegating a sub-task to another agent (handoff)

`handoff` is the **cross-agent** counterpart to `rotate`. Where `rotate` retires
**you** in favour of a same-worktree successor, `handoff` hands a slice of work to
a **different** agent and **you keep running**. Use it to fork off an independent
sub-task or to brief an already-running agent.

Two modes:
- **New delegate (default):** spawns a fresh agent with **its own isolated
  worktree** off the repo. Best for a sub-task that can proceed in parallel.
- **Existing agent (`--to <id>`):** delivers the handoff into a running agent's
  inbox (waking it if idle). Best for briefing an agent already on a related task.

Same two-phase, human-reviewed shape as rotate:

1. **Prepare (you do this directly).** Write a handoff file to a unique per-agent
   temp path (`${TMPDIR:-/tmp}/warden-handoff-$WARDEN_SESSION_ID.md`). The
   recipient runs in a *different* worktree, so write **self-contained** context —
   its content is inlined into the recipient's prompt/message, not read by path.
   Compose a one-paragraph resume prompt. Show both and **stop** for review.

2. **Deliver (after the user says go):**

   ```sh
   HANDOFF="${TMPDIR:-/tmp}/warden-handoff-$WARDEN_SESSION_ID.md"
   # New delegate (its own worktree off your repo):
   warden handoff --resume-file "$HANDOFF" --resume-prompt "<task>"
   # …or hand to an existing agent:
   warden handoff --to <agent-id> --resume-file "$HANDOFF" --resume-prompt "<task>"
   ```

   New mode prints the delegate's id (`warden attach <id>` to watch it); `--to`
   mode confirms delivery and whether the recipient was woken. **You are never
   retired** — this is delegation, not succession. If new-mode spawn is blocked by
   the memory-pressure gate, add `--force` to spawn anyway.
