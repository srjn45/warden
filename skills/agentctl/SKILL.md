---
name: agentctl
description: Use to spawn, list, monitor, talk to, and tear down Claude Code agent sessions via agentctl. Triggers include "spawn/create an agent", "list/check/triage my agents", "what is agent <id> doing", "tell/ask agent <id> to …", "send to an agent", "terminate/kill/remove agent(s)", "manage my agents". Drives the agentctl MCP tools or the agentctl CLI — both are first-class paths.
---

# agentctl — drive your agent fleet

agentctl runs a local daemon that spawns and monitors per-task Claude Code agents
(each in its own tmux session, some in a git worktree). Use this skill to manage
them on the user's behalf through the **agentctl MCP tools**:
`list_agents`, `get_agent`, `spawn_agent`, `send_to_agent`, `get_agent_output`,
`terminate_agent`, `delete_agent`, `remove_worktree`.

## Preconditions

- The daemon must be running. If a tool returns a connection / "daemon not
  running" error, tell the user to start it (`agentctl daemon`, or via launchd) —
  do not guess at agent state.
- Two equivalent ways to drive the fleet: the **agentctl MCP tools** (when registered in this session) and the **`agentctl` CLI** (always available wherever the binary is installed). Both wrap the same daemon REST API — use whichever is available. CLI verbs: `agentctl ls`, `agentctl start "<prompt>"`, `agentctl send <id> "<msg>"`, `agentctl tail <id>`, `agentctl terminate <id>`, `agentctl delete <id>`, `agentctl remove-worktree <id>` (and `agentctl done <id>` = terminate + delete). **Note:** MCP registration may be blocked by enterprise policy (`claude mcp add` is locked down on some machines); when the MCP tools are absent, use the CLI — no capability is lost.

## Tool argument note

The per-agent tools (`get_agent`, `send_to_agent`, `get_agent_output`,
`terminate_agent`, `delete_agent`, `remove_worktree`) take a `ticket` argument —
this is the agent's **id** as shown by `list_agents` (for prompt-spawned agents it
looks like `agent-<shortid>`). Pass that id as the `ticket` value.

## Intent → action

| The user wants to… | Do this |
|---|---|
| list / check / triage agents | `list_agents`; summarize by status. Call out `waiting_for_input` (needs them) and `errored`/`orphaned`. Show each agent's `subject` and `workdir`. |
| spin up / create an agent to do X | `spawn_agent` with `prompt: "X"` (auto-typed, no repo needed). Only use `type`+`repo` (+`branch`/`pr`/`worktree`) when the user explicitly wants a managed worktree tied to a repo/ticket. |
| what is agent <id> doing / its status | `get_agent` (status, subject, workdir, event history) + `get_agent_output` (recent terminal) → report concisely in plain language. |
| tell / ask agent <id> to do Y | `send_to_agent` (the id as `ticket`, plus `text`). Echo back what you sent. |
| stop / terminate / kill <id> | `terminate_agent` (id) — kills tmux+claude, keeps record+worktree; reversible via `restore_agent` |
| clear / delete an agent's record | `delete_agent` (id, hard?) — archives by default |
| remove an agent's worktree | `remove_worktree` (id, force?) — DESTRUCTIVE; **confirm with the user first**; terminate the agent first |
| restore / bring back a lost or orphaned agent | `restore_agent` (id) — only for sessions whose tmux is gone (status `orphaned`); resumes the same conversation |
| adopt / register an existing Claude session | `adopt_agent` (dir?, session_id?, tmux_session?) — resume the newest conversation for a dir under tmux (plain shell), or register a running tmux session live (pass `tmux_session`) |

## CLI command map (when not using MCP tools)

| Intent | CLI command |
|---|---|
| list / triage agents | `agentctl ls` (add `--json` for machine-readable output) |
| full status of one agent | `agentctl status <id>` (add `--json` for the full object incl. events) |
| recent terminal output | `agentctl tail <id>` |
| spawn from a prompt | `agentctl start "<prompt>"` |
| spawn a managed worktree agent | `agentctl start <TICKET> --type <TYPE> --repo <repo>` |
| send a message to an agent | `agentctl send <id> "<text>"` |
| terminate / clean up | `agentctl done <id>` (guarded; `--force` to override the git guard) |
| restore a lost/orphaned agent | `agentctl restore <id>` |
| adopt / register an existing session | `agentctl adopt [--session-id <uuid>] [--dir <path>]` |
| attach interactively | `agentctl attach <id>` |

Prefer `--json` on `ls`/`status` when you need to parse the result programmatically — the table/text views are for humans and may change.

## Guardrails

- **`remove_worktree` is destructive — always confirm with the user first** (name the agent + that its worktree and branch will be deleted); it refuses while the agent is still running or has uncommitted/unpushed work unless `force:true`. `terminate_agent` is the safe default for 'stop this agent' (reversible via `restore_agent`). Never bulk terminate/delete/remove without explicit per-agent or explicit 'all' confirmation.
- Never fabricate agent state. Always read it via `list_agents` / `get_agent` /
  `get_agent_output`.
- When the daemon is unreachable, say so plainly and stop — don't invent results.
- Restore is resume-only and for `orphaned`/dead sessions; if it reports the agent is still running, use `send`/attach instead.

## Examples

- "Spin up an agent to investigate the flaky auth test."
  → `spawn_agent {prompt: "investigate the flaky auth test and propose a fix"}`,
    then report the new agent id.
- "What's agent-4f2a up to?"
  → `get_agent {ticket: "agent-4f2a"}` + `get_agent_output {ticket: "agent-4f2a"}`
    → "It's analysing the auth module; last output shows it running the test suite."
- "Tell it to also check the refresh-token path."
  → `send_to_agent {ticket: "agent-4f2a", text: "also check the refresh-token path"}`.
- "Kill the idle ones."
  → `list_agents`, identify the `idle`/`done` agents, list them and ask the user to
    confirm, then `terminate_agent` each confirmed id.
