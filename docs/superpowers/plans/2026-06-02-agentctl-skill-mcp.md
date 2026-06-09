# agentctl Claude Skill + MCP Polish Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let any Claude session drive agentctl conversationally — add a `prompt` field to the MCP `spawn_agent` tool (so agents can be created from a prompt, auto-typed), and ship a packaged Claude Code skill (`skills/agentctl/SKILL.md` + `make install-skill`) that teaches Claude to list/triage, create, inspect ("what is X doing"), relay to, and terminate agents via the MCP tools (CLI fallback), with safety guardrails.

**Architecture:** Additive. One MCP arg + handler line (`spawn_agent` gains `prompt`, mapped to `client.SpawnParams.Prompt`; the daemon already accepts *prompt* OR *type+repo*). A prose `SKILL.md` driving the existing six MCP tools. A symlink installer + README section.

**Tech Stack:** Go 1.26 (`internal/mcp` with the MCP Go SDK), Markdown (the skill), Makefile.

**Reference spec:** `docs/superpowers/specs/2026-06-02-agentctl-skill-mcp-design.md`

---

## Conventions
- Module `github.com/srajanpathak/agentctl`. Executor sets up an isolated worktree first.
- Go: TDD. Commit after each task with the given message (no Co-Authored-By footer).
- Go tests `go test ./internal/mcp/` (no Docker; the test stands up an httptest fake daemon + in-memory MCP transport).

## File map
```
internal/mcp/server.go       spawnArgs +Prompt; spawn_agent handler maps it; description update
internal/mcp/server_test.go  + TestSpawnAgentToolSendsPrompt
skills/agentctl/SKILL.md      the Claude Code skill (orchestration + relay + guardrails)
Makefile                      + install-skill target
README.md                     + "Drive agentctl from Claude (skill + MCP)" section
```

Phase order: **1** MCP prompt support → **2** the skill + installer → **3** README + integration.

---

## Phase 1 — MCP: spawn_agent accepts a prompt

### Task 1.1: Add `prompt` to spawn_agent

**Files:** `internal/mcp/server.go`, `internal/mcp/server_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/mcp/server_test.go` (it already imports `context`, `net/http`, `net/http/httptest`, `testing`, the MCP SDK as `mcpsdk`, and testify; add `"io"` if not already imported):
```go
func TestSpawnAgentToolSendsPrompt(t *testing.T) {
	var gotBody string
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/spawn" {
			b, _ := io.ReadAll(r.Body)
			gotBody = string(b)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"agent-x","status":"spawning"}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer daemon.Close()

	srv := NewServer(daemon.URL)
	ctx := context.Background()
	clientTransport, serverTransport := mcpsdk.NewInMemoryTransports()
	go func() { _ = srv.Run(ctx, serverTransport) }()

	cl := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "0"}, nil)
	session, err := cl.Connect(ctx, clientTransport, nil)
	require.NoError(t, err)
	defer session.Close()

	res, err := session.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "spawn_agent",
		Arguments: map[string]any{"prompt": "research SSE reconnection"},
	})
	require.NoError(t, err)
	require.False(t, res.IsError, textOf(res))
	require.Contains(t, gotBody, `"prompt":"research SSE reconnection"`)
}
```
(`textOf` already exists in `server_test.go` from the MCP build. If the installed SDK's `CallToolParams` field for arguments is named differently than `Arguments`, mirror whatever the existing `TestListAgentsTool` / SDK uses — keep the asserted behavior identical.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/mcp/ -run TestSpawnAgentToolSendsPrompt`
Expected: FAIL — the spawn body has no `"prompt"` (the field isn't in `spawnArgs` / isn't forwarded).

- [ ] **Step 3: Add the field + map it**

In `internal/mcp/server.go`, add `Prompt` to `spawnArgs` (after `Worktree`):
```go
	Prompt   string `json:"prompt" jsonschema:"what the agent should do — prompt-mode: auto-typed, no repo needed"`
```
Update the `spawn_agent` handler's `client.SpawnParams` to forward it:
```go
		sess, err := s.cl.Spawn(ctx, client.SpawnParams{
			Type: a.Type, Ticket: a.Ticket, Repo: a.Repo,
			Branch: a.Branch, PR: a.PR, Worktree: a.Worktree,
			Prompt: a.Prompt,
		})
```
Update the tool `Description` to explain both modes:
```go
		Description: "Spawn an agent. Provide `prompt` for a quick auto-typed agent (no repo needed). OR provide `type`+`repo` for a managed worktree (development/pr-review get a worktree; buildkite-debug/test-run/env-test run in the repo; analysis/spike take an optional worktree). Launches claude --dangerously-skip-permissions.",
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/mcp/`
Expected: PASS (the new test + the existing MCP tests).

- [ ] **Step 5: Commit**

```bash
git add internal/mcp/server.go internal/mcp/server_test.go
git commit -m "feat: MCP spawn_agent accepts a prompt (auto-typed prompt-spawn)"
```

---

## Phase 2 — The skill + installer

### Task 2.1: Write `skills/agentctl/SKILL.md`

**Files:** `skills/agentctl/SKILL.md`

- [ ] **Step 1: Create the skill**

Create `skills/agentctl/SKILL.md` with exactly this content:
```markdown
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
  `agentctl` CLI: `agentctl ls`, `agentctl start "<prompt>"`, `agentctl send <id> "<msg>"`,
  `agentctl tail <id>`, `agentctl done <id>`.

## Intent → action

| The user wants to… | Do this |
|---|---|
| list / check / triage agents | `list_agents`; summarize by status. Call out `waiting_for_input` (needs them), and `errored`/`orphaned`. Show each agent's `subject` and `workdir`. |
| spin up / create an agent to do X | `spawn_agent` with `prompt: "X"` (auto-typed, no repo needed). Only use `type`+`repo` (+`branch`/`pr`/`worktree`) when the user explicitly wants a managed worktree tied to a repo/ticket. |
| what is agent <id> doing / its status | `get_agent` (status, subject, workdir, event history) + `get_agent_output` (recent terminal) → report concisely in plain language. |
| tell / ask agent <id> to do Y | `send_to_agent` (id, text). Echo back what you sent. |
| terminate / kill / clean up <id> | `cleanup_agent` — **see Guardrails**. |

## Guardrails

- **`cleanup_agent` is destructive — always confirm first.** Name the agent(s) and
  what will be removed (tmux session; for worktree agents, the worktree + branch).
- Default to the **guarded** cleanup (no force). If it reports a conflict
  (uncommitted or unpushed changes), explain that and only retry with `force: true`
  if the user explicitly accepts losing that work.
- **Never bulk-terminate** without explicit confirmation — either per agent, or an
  explicit "yes, all of them".
- Never fabricate agent state. Always read it via `list_agents` / `get_agent` /
  `get_agent_output`.
- When the daemon is unreachable, say so plainly and stop — don't invent results.

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
  → `list_agents`, identify `idle`/`done` agents, list them and ask the user to
    confirm, then `cleanup_agent` each confirmed id (guarded).
```

> Note on tool argument names: the agentctl MCP tools that target one agent take a
> `ticket` argument (the agent id — for prompt-spawned agents this is `agent-<shortid>`).
> Use the agent's `id` from `list_agents` as that `ticket` value. (This matches the
> existing `get_agent`/`send_to_agent`/`get_agent_output`/`cleanup_agent` arg schema.)

- [ ] **Step 2: Verify frontmatter is well-formed**

Run:
```bash
head -5 skills/agentctl/SKILL.md
python3 -c "import sys,re; t=open('skills/agentctl/SKILL.md').read(); m=re.match(r'^---\n(.*?)\n---\n', t, re.S); assert m, 'no frontmatter'; import yaml; d=yaml.safe_load(m.group(1)); assert d.get('name')=='agentctl' and d.get('description'); print('frontmatter OK:', d['name'])" 2>/dev/null || grep -qE '^name: agentctl$' skills/agentctl/SKILL.md && echo "frontmatter OK"
```
Expected: prints `frontmatter OK` (the fallback `grep` covers environments without PyYAML).

- [ ] **Step 3: Commit**

```bash
git add skills/agentctl/SKILL.md
git commit -m "feat: agentctl Claude Code skill (orchestration + relay + guardrails)"
```

### Task 2.2: `make install-skill`

**Files:** `Makefile`

- [ ] **Step 1: Add the target**

In `Makefile`, add `install-skill` to `.PHONY` and append the target:
```make
install-skill:
	mkdir -p ~/.claude/skills
	ln -sfn $(PWD)/skills/agentctl ~/.claude/skills/agentctl
	@echo "linked ~/.claude/skills/agentctl -> $(PWD)/skills/agentctl"
```

- [ ] **Step 2: Verify the symlink resolves to the repo skill**

Run:
```bash
make install-skill
readlink ~/.claude/skills/agentctl
test "$(readlink ~/.claude/skills/agentctl)" = "$(PWD)/skills/agentctl" && echo "symlink OK"
test -f ~/.claude/skills/agentctl/SKILL.md && echo "SKILL.md reachable"
```
Expected: `symlink OK` and `SKILL.md reachable`. (Idempotent — re-running `make install-skill` is safe via `ln -sfn`.)

- [ ] **Step 3: Commit**

```bash
git add Makefile
git commit -m "chore: make install-skill (symlink the skill into ~/.claude/skills)"
```

---

## Phase 3 — README + integration

### Task 3.1: README + full verification

**Files:** `README.md`

- [ ] **Step 1: Full build + suites**

Run:
```bash
make release && make mongo-up
go build ./... && go vet ./... && go test ./...
```
Expected: all green (incl. `internal/mcp` with the new prompt test).

- [ ] **Step 2: README section**

In `README.md`, add a "Drive agentctl from Claude (skill + MCP)" section documenting:
- Register the `agentctl` MCP server in your Claude session (point to the existing `mcpServers` snippet already in the README: command `agentctl`, args `["mcp"]`, env `AGENTCTL_ADDR`).
- Run `make install-skill` to symlink the `agentctl` skill into `~/.claude/skills/`.
- Then any Claude session can manage the fleet conversationally: "list my agents", "spin up an agent to research X", "what is agent-4f2a doing?", "tell agent-4f2a to run the tests", "kill the idle ones" — Claude uses the MCP tools (or the `agentctl` CLI as fallback).
- Note `spawn_agent` now takes a `prompt` (auto-typed) as well as the typed `type`+`repo` form.
- Note the daemon must be running.

- [ ] **Step 3: Commit**

```bash
git add README.md
git commit -m "docs: drive agentctl from Claude (skill + MCP)"
```

- [ ] **Step 4: Manual smoke (optional, needs a live session)**

Register the MCP server + `make install-skill`, then in a Claude session say "list my agents" and "spin up an agent to research X" and confirm it calls `list_agents` / `spawn_agent {prompt}`. (Sandbox can't drive an interactive Claude session; the Go test covers the MCP prompt path.)

---

## Self-review against the spec

**Spec coverage** (`2026-06-02-agentctl-skill-mcp-design.md`):
- §2/§3 MCP `spawn_agent` gains `prompt`, mapped to `client.SpawnParams.Prompt`, typed args kept, description updated — Task 1.1. ✅
- §2/§4 skill `skills/agentctl/SKILL.md` (frontmatter triggers; intent→tool table; create-from-prompt default; "what is X doing" via get_agent+output; relay via send_to_agent; terminate via cleanup_agent) — Task 2.1. ✅
- §4 guardrails (confirm before terminate, guarded-by-default + force only on explicit accept, no bulk-kill without confirmation, never fabricate state, daemon-down handling) — Task 2.1 (Guardrails section). ✅
- §2/§5 install: in-repo skill + `make install-skill` symlink to `~/.claude/skills/agentctl` (idempotent) — Task 2.2. ✅
- §5 README "drive from Claude" section (MCP registration + install-skill + examples + prompt note) — Task 3.1. ✅
- §6 testing: Go MCP prompt test (in-memory client → `spawn_agent {prompt}` → fake daemon receives `/spawn` with the prompt); existing MCP/daemon tests stay green; frontmatter + symlink sanity checks — Tasks 1.1, 2.1, 2.2. ✅
- §7 out of scope respected (no new tools beyond the field; no auto-register/auto-start). ✅

**Placeholder scan:** No TBD/TODO. The SKILL.md is provided in full; every code/command step has exact content + expected output. Task 1.1 Step 1 flags the one external unknown (the SDK's `CallToolParams` arguments field name) with explicit guidance to mirror the existing `TestListAgentsTool`/SDK shape.

**Type consistency:** `spawnArgs.Prompt` (`json:"prompt"`) flows into `client.SpawnParams.Prompt` (which exists and the daemon already handles — verified against `internal/client/client.go` and the prompt-spawn feature). The per-agent MCP tools take a `ticket` arg = the agent id (matches the existing `get_agent`/`send_to_agent`/`get_agent_output`/`cleanup_agent` schemas), and the SKILL.md's note documents that mapping so Claude passes `id` as `ticket`. `make install-skill` symlinks `skills/agentctl` (the dir Task 2.1 creates) into `~/.claude/skills/agentctl`.
