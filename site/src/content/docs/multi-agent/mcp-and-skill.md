---
title: "Orchestration: MCP & skill"
description: Register warden as an MCP server so an orchestrator Claude can manage the fleet, and install the /warden skill.
---

`warden mcp` is a stdio MCP server so an orchestrator Claude session can manage the fleet through tool calls.

## Register the MCP server

Register `warden mcp` as an MCP server in your orchestrator Claude session's MCP config (e.g. `~/.claude/claude_desktop_config.json` or the project-level `.claude/mcp.json`):

```json
{
  "mcpServers": {
    "warden": {
      "command": "warden",
      "args": ["mcp"],
      "env": {
        "WARDEN_ADDR": "127.0.0.1:8765"
      }
    }
  }
}
```

## Tools exposed

| Tool | Purpose |
|---|---|
| `list_agents` / `get_agent` | List agents / full detail for one |
| `spawn_agent` | Spawn (prompt mode or `type`+`repo`; `supervised` opt-in) |
| `adopt_agent` | Register an existing Claude session |
| `send_to_agent` / `get_agent_output` | Type into / read recent output of an agent |
| `terminate_agent` / `restore_agent` | Stop (reversible) / resume an agent |
| `delete_agent` / `remove_worktree` | Clear record / remove worktree (guarded) |
| `ctx_set` / `ctx_get` / `ctx_list` | Shared-context blackboard |
| `ctx_cas` / `ctx_append` | Compare-and-set / append-to-list context writes (lock-free coordination) |
| `send_message` / `read_inbox` / `wait_for_message` | Directed messaging, incl. a blocking long-poll wait |
| `list_approvals` / `approve` | List / answer pending tool-permission prompts |
| `commit` / `push` / `sync` / `check` | The git + check lifecycle on an agent's branch (warden rails) |
| `get_collaboration_status` / `who_is_editing_file` | See which agents are editing the same files |
| `create_pipeline` / `start_pipeline` | Author (from YAML or a built-in template) and launch a DAG pipeline |
| `show_pipeline` / `list_pipelines` / `cancel_pipeline` | Inspect / list / cancel pipelines |

> Pipelines **can** be driven over MCP: `create_pipeline`, `start_pipeline`, `show_pipeline`, `list_pipelines`, and `cancel_pipeline` are all tools. The finer-grained controls — `pause`/`resume`/`delete`/`edit-job`/`retry` — remain CLI-only (`warden pipeline …`).

Example orchestrator prompts:

- *"What is PROJ-350 doing?"* → `get_agent`
- *"Tell PROJ-343 to run the tests"* → `send_to_agent`
- *"List all my agents"* → `list_agents`
- *"Spin up an agent to research SSE reconnection"* → `spawn_agent` with a `prompt` (auto-typed)
- *"Spawn a debug-ci agent in /path/to/repo"* → `spawn_agent` with `type`+`repo`
- *"Stop PROJ-350"* → `terminate_agent` (reversible); "clear its record too" → `delete_agent`
- *"Kick off the analyze-implement-review pipeline on /path/to/repo"* → `create_pipeline` (template) + `start_pipeline`
- *"Commit and push agent-4f2a's branch"* → `commit` then `push`; *"is anyone else editing auth.go?"* → `who_is_editing_file`

> Prefer natural language over tool calls? `warden orch` is a local-LLM conductor REPL that drives these same operations from plain English without an orchestrator Claude session — see [Orchestrator REPL](/warden/multi-agent/orchestrator-repl/).

## The `/warden` Claude skill

A packaged Claude Code skill teaches any Claude session *how and when* to manage the fleet (triage, create-from-prompt, relay "tell X to do Y", terminate-with-confirmation, daemon-down handling, self-rotation). It drives the MCP tools, falling back to the `warden` CLI when the MCP server isn't registered.

```sh
make install-skill   # symlinks skills/warden into ~/.claude/skills/warden
```

With the MCP server registered (above) and the skill installed, just talk to a Claude session: *"list my agents"*, *"spin up an agent to research X"*, *"what is agent-4f2a doing?"*, *"tell agent-4f2a to run the tests"*, *"kill the idle ones"* — it drives the MCP tools (falling back to the `warden` CLI if the MCP server isn't registered). The daemon must be running.
