---
name: agentctl
description: Use to spawn, list, monitor, talk to, and tear down Claude Code agent sessions via agentctl. Triggers include "spawn/create an agent", "list/check/triage my agents", "what is agent <id> doing", "tell/ask agent <id> to …", "send to an agent", "terminate/kill/clean up agent(s)", "manage my agents". Drives the agentctl MCP tools (with the agentctl CLI as a fallback).
---

# agentctl — drive your agent fleet

agentctl runs a local daemon that spawns and monitors per-task Claude Code agents
(each in its own tmux session, some in a git worktree). Use this skill to manage
them on the user's behalf through the **agentctl MCP tools**:
`list_agents`, `get_agent`, `spawn_agent`, `send_to_agent`, `get_agent_output`,
`cleanup_agent`.

## Preconditions

- The daemon must be running. If a tool returns a connection / "daemon not
  running" error, tell the user to start it (`agentctl daemon`, or via launchd) —
  do not guess at agent state.
- If the `agentctl` MCP tools are not available in this session, fall back to the
  `agentctl` CLI: `agentctl ls`, `agentctl start "<prompt>"`,
  `agentctl send <id> "<msg>"`, `agentctl tail <id>`, `agentctl done <id>`.

## Tool argument note

The per-agent tools (`get_agent`, `send_to_agent`, `get_agent_output`,
`cleanup_agent`) take a `ticket` argument — this is the agent's **id** as shown by
`list_agents` (for prompt-spawned agents it looks like `agent-<shortid>`). Pass
that id as the `ticket` value.

## Intent → action

| The user wants to… | Do this |
|---|---|
| list / check / triage agents | `list_agents`; summarize by status. Call out `waiting_for_input` (needs them) and `errored`/`orphaned`. Show each agent's `subject` and `workdir`. |
| spin up / create an agent to do X | `spawn_agent` with `prompt: "X"` (auto-typed, no repo needed). Only use `type`+`repo` (+`branch`/`pr`/`worktree`) when the user explicitly wants a managed worktree tied to a repo/ticket. |
| what is agent <id> doing / its status | `get_agent` (status, subject, workdir, event history) + `get_agent_output` (recent terminal) → report concisely in plain language. |
| tell / ask agent <id> to do Y | `send_to_agent` (the id as `ticket`, plus `text`). Echo back what you sent. |
| terminate / kill / clean up <id> | `cleanup_agent` — **see Guardrails**. |
| restore / bring back a lost or orphaned agent | `restore_agent` (id) — only for sessions whose tmux is gone (status `orphaned`); resumes the same conversation |

## Guardrails

- **`cleanup_agent` is destructive — always confirm first.** Name the agent(s) and
  what will be removed (the tmux session; for worktree agents, the worktree + branch).
- Default to the **guarded** cleanup (no force). If it reports a conflict
  (uncommitted or unpushed changes), explain that and only retry with `force: true`
  if the user explicitly accepts losing that work.
- **Never bulk-terminate** without explicit confirmation — either per agent, or an
  explicit "yes, all of them".
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
    confirm, then `cleanup_agent` each confirmed id (guarded).
