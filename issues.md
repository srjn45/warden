# Warden issues — week of Mon Jun 30 → Fri Jul 3, 2026

Log-based summary of problems observed this week: instability delegating
agents, backpressure, and Claude being unable to use the warden MCP (falling
back to built-in subagents).

**Sources:** `~/.warden/audit.jsonl`, daemon log `/tmp/warden.daemon.err`,
`~/.warden/memlog/`, `~/.warden/config.yaml`, `orch_history`, and the enterprise
`managed-mcp.json`.
_Note: `/tmp/warden.daemon.log` is empty — the daemon writes all structured logs to stderr._

---

## 1. Instability delegating / spawning agents

- **28 spawns and 28 terminates** this week, of which **7 died within 90s of spawn**:
  - Mon Jun 30 morning cluster (13–24s lifetimes): `agent-42b16865` 20s,
    `9db54e8e` 17s, `e41742c6` 17s, `27876766` 24s, `d9d94be4` 14s, `255e8b6a` 17s
  - Tue Jul 1: `agent-57e34b7a` **3.1s**
- `orch_history` shows the **same "spawn an agent in all-the-things to list
  card-simulator APIs" flow retried 3+ times** with different names (`cs-apis`,
  `cs-api-list`) and abandoned `exit`s — the spawn UX was being fought with, not
  working first try.
- **Daemon restarted 9 times**, almost all clustered on Jul 1
  afternoon/evening (16:16, 16:32, 16:34, 16:58, 17:36, 18:26, 19:59, 23:58) —
  one restart preceded by `error: context deadline exceeded`.
- **Stuck / looping agents:**
  - `agent-31336489` auto-approved the *identical* `aws sts get-caller-identity`
    prompt ~every 10s for hours (blocked on ECR/onelogin auth).
  - `agent-bad1758b` triggered a **"possible infinite loop"** alert Jul 2 16:38.

## 2. Backpressure — two distinct layers

### a) System memory pressure (the machine, not warden)
- Jul 1 overnight: **free RAM collapsed to 554–822 MB**, memory compressor
  pinned at ~7.7 GB, anon 24 GB.
- Jul 2: **swap 2.2 GB**, load spiking to **15–22**.
- Dozens of memlog gaps of **~900–1600s labeled "sampler stalled — possible
  freeze"** — the machine was thrashing / near-frozen. This is exactly what
  `worktree.spawn_gate: true` / `spawn_gate_max_agents: 10` is meant to guard.

### b) Warden's internal slow-route / summarizer backpressure
- The local-LLM summarizer (ollama) **failed ~50 times on Jul 1** with
  `context deadline exceeded` (`qwen3:14b`), plus a hard
  `404 model 'qwen3.5:2b' not found`. Every failure **fell back to Claude**
  for status summaries — slower and token-costly.
- Now `local_llm.enabled: false` in config and `warden doctor` reports
  **ollama not on PATH**, so summaries run degraded / Claude-only.

## 3. Claude can't use warden → forced onto built-in agents

- The **enterprise `managed-mcp.json`** (root-owned, MDM-pushed at
  `/Library/Application Support/ClaudeCode/`) defines an **exclusive MCP
  allowlist** — memory-bank, notion, jira, buildkite, snowflake, chronosphere,
  sentry, playwright, salesforce, etc. **warden is not in it.**
- `claude mcp list` shows **no warden entry**, and sessions expose only the
  warden *skill* + CLI, **no `mcp__warden__*` tools**.
- Result: Claude cannot call warden's MCP tools and falls back to the built-in
  `Task` / `Agent` subagent tool. This is a **policy / registration issue, not a
  warden bug** — skill + CLI are the working path.

## 4. Context-window pressure (secondary)

- Repeated **"context high" alerts** — `de049cc0` at 158k then 151k,
  `fc3d8194` at 150k then **200k**, `bad1758b` at 152k — long-lived agents
  ballooning toward the guard's `critical: 350000`.

---

## Bottom line

Three independent failure modes stacked this week:

1. **Delegation was flaky** — short-lived spawns + 9 daemon restarts + retried
   spawn flows + two stuck/looping agents.
2. **Backpressure was real and dual** — the Mac was near-OOM / thrashing
   (spawn-gate territory), *and* warden's local-LLM summarizer was timing out
   into Claude fallback (now disabled).
3. **The warden MCP is blocked by the exclusive managed-MCP policy**, so Claude
   is forced to use its built-in subagents instead of warden.
