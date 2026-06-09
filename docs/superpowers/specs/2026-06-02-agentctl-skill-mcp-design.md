# agentctl Claude Skill + MCP Polish — Design

**Date:** 2026-06-02
**Status:** Approved design (pre-implementation)
**Owner:** Srajan Pathak (personal project)
**Sub-project C** (final) of the terminal-first / Claude-integrated direction (B context ✅ → A TUI ✅ → **C skill+MCP**).

---

## 1. Goal

Let any Claude session drive agentctl conversationally — "spin up an agent to research X", "what's agent-4f2a doing?", "tell it to run the tests", "list my agents", "kill the idle ones" — via a packaged Claude Code **skill** that wraps the existing `agentctl` **MCP** tools (with the CLI as fallback). Plus one MCP gap fix so agents can be created from a prompt.

## 2. Key decisions

| Decision | Choice |
|---|---|
| MCP spawn gap | Add a `prompt` field to the `spawn_agent` tool (prompt-mode → auto-typed). Keep the typed `type/repo/branch/pr/worktree` args (daemon accepts *prompt* OR *type+repo*). |
| Skill packaging | In-repo `skills/agentctl/SKILL.md`; `make install-skill` symlinks it into `~/.claude/skills/agentctl/` (idempotent; in-repo edits reflect live). |
| Skill scope | Full orchestration + relay with guardrails (list/triage, create-from-prompt, "what is X doing", relay to X, terminate-with-confirm). |
| Primary mechanism | The `agentctl` MCP tools; CLI (`agentctl ls/start/send/tail/done`) documented as the fallback when MCP isn't registered. |

## 3. MCP polish

`internal/mcp/server.go`:
- `spawnArgs` gains `Prompt string` (`json:"prompt"`, jsonschema: "what the agent should do — prompt-mode, auto-typed, no repo needed"). Existing fields unchanged.
- The `spawn_agent` handler maps it: `client.SpawnParams{Prompt: a.Prompt, Type: a.Type, Ticket: a.Ticket, Repo: a.Repo, Branch: a.Branch, PR: a.PR, Worktree: a.Worktree}` (the client/daemon already validate prompt OR type+repo).
- Tool `Description` updated to explain the two ways to spawn.
- No other tool changes — `list_agents`/`get_agent` already serialize the full `Session` (so `workdir`/`subject` flow); `send_to_agent`/`get_agent_output`/`cleanup_agent` are complete.

## 4. The skill — `skills/agentctl/SKILL.md`

Frontmatter:
```yaml
---
name: agentctl
description: Use to spawn, list, monitor, talk to, and tear down Claude Code agent sessions via agentctl — triggers include "spawn/create an agent", "list/check my agents", "what is agent <id> doing", "tell agent <id> to …", "send to an agent", "terminate/kill agent(s)", "manage my agents". Drives the agentctl MCP tools (CLI fallback).
---
```

Body (sections):
- **What agentctl is** — a local daemon managing per-task Claude Code agents (tmux + optional worktree); this skill drives it through the `agentctl` MCP tools.
- **Preconditions** — the daemon must be running; if any tool reports a daemon-down/connection error, tell the user to start it (`agentctl daemon`, or via launchd). If the `agentctl` MCP tools aren't available in the session, fall back to the `agentctl` CLI (`ls`, `start "<prompt>"`, `send`, `tail`, `done`).
- **Intent → tool** table:
  | User asks | Do |
  |---|---|
  | list / check / triage agents | `list_agents` → summarize by status; flag `waiting_for_input` (needs you), `errored`/`orphaned` |
  | spin up / create an agent to do X | `spawn_agent` with `prompt: "X"` (auto-typed); use `type`+`repo` only for an explicit managed-worktree task |
  | what is agent X doing / status | `get_agent` (status, subject, dir, events) + `get_agent_output` (recent terminal) → report concisely |
  | tell / ask agent X to do Y | `send_to_agent` (id, text) |
  | terminate / kill / clean up X | `cleanup_agent` — **confirm first**; guarded (non-force) by default |
- **Guardrails** — `cleanup_agent` is destructive: always confirm before terminating; never bulk-terminate without explicit per-agent or explicit "all" confirmation; default to the guarded cleanup, only `force: true` when the user explicitly accepts losing uncommitted/unpushed work, and explain the guard if a 409 trips; never fabricate agent state — always read it via the tools; report daemon-down plainly instead of guessing.
- **Examples** — 2–3 short exchanges (create-from-prompt; "what's X doing"; "tell X to run tests"; "kill the idle ones" → list, confirm, cleanup each).

## 5. Install

- `Makefile` target:
  ```make
  install-skill:
  	mkdir -p ~/.claude/skills
  	ln -sfn $(PWD)/skills/agentctl ~/.claude/skills/agentctl
  ```
- README: a "Drive agentctl from Claude (skill + MCP)" section — register the `agentctl` MCP server (already documented), run `make install-skill`, then talk to any Claude session ("list my agents", "spin up an agent to …", "what is agent X doing", "tell X to …", "kill the idle ones").

## 6. Components & testing

- **Code:** `internal/mcp/server.go` (+`prompt` on `spawn_agent`), `internal/mcp/server_test.go` (+spawn-with-prompt test).
- **Assets/docs:** `skills/agentctl/SKILL.md`, `Makefile` (`install-skill`), `README.md`.
- **Testing:**
  - Go: an in-memory MCP client calls `spawn_agent` with `{prompt:"…"}`; assert the fake daemon receives a `POST /spawn` whose body carries that prompt. Existing MCP + daemon tests stay green.
  - Skill: SKILL.md has valid frontmatter (`name`, `description`); `make install-skill` creates a symlink that resolves to the repo's `skills/agentctl` (verified by `readlink`). The SKILL.md is reviewed for clarity + that the safety guardrails (confirm-before-terminate, no bulk-kill, daemon-down handling) are present — it's a prose doc, no unit test.

## 7. Out of scope
- New MCP tools beyond the `prompt` field (the existing six cover the surface).
- Auto-registering the MCP server or auto-starting the daemon (documented, not automated).
- Multi-session/remote orchestration.
