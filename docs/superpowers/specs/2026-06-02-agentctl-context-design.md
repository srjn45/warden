# agentctl Richer Context (workdir + auto-subject) — Design

**Date:** 2026-06-02
**Status:** Approved design (pre-implementation)
**Owner:** Srajan Pathak (personal project)
**Sub-project B** of the terminal-first / Claude-integrated direction (B → A TUI → C skill+MCP). Extends the prompt-spawn design.

---

## 1. Goal

Make it clear, per agent, **which directory it's working in** and **what it's about / doing**, surfaced in `ls`, `status`, the GUI, and over MCP. Deep Claude integration is allowed: read Claude Code transcripts and shell `claude -p`.

## 2. Key decisions

| Decision | Choice |
|---|---|
| Per-agent workdir | Prompt-spawned agents run in their **own** dir `~/agentctl-agents/<id>/` (created at spawn). Makes "which dir" unambiguous AND gives each agent its own Claude transcript project dir. Typed/worktree agents keep their existing unique dir. |
| Workdir storage | New `Session.workdir` (absolute cwd), set at spawn. |
| Subject source | **Hybrid:** the agent's Claude transcript when locatable (by encoded workdir), else fall back to the tmux pane. |
| Subject generation | `claude -p` over the recent transcript/pane text → a ≤8-word phrase. |
| Cadence | **Seed** at spawn (truncated prompt, no Claude call) + **throttled refresh** by the poller (only when the pane changed AND at most every `AGENTCTL_SUMMARIZE_AFTER`, default 2m). Pushed live via SSE. |
| Display | `ls` gains `DIR` + `SUBJECT` columns; `status`/GUI show full workdir + subject; MCP serializes them automatically. |

## 3. Per-agent working directory

In `lifecycle.Spawn` **prompt mode**, the working directory becomes `<req.Workdir>/<id>` (e.g. `~/agentctl-agents/agent-a1b2/`), created with `mkdir -p` via the mockable `Runner` before `tmux new-session -c <dir>`. `sess.Workdir` is set to that absolute path.

In **typed mode**, `sess.Workdir` is set to the dir the session already runs in (the repo, or the absolute worktree path when a worktree is created). No behavior change beyond recording it.

Rationale: a unique cwd per agent means its Claude transcript lives in a unique `~/.claude/projects/<encoded-cwd>/` directory, so the newest `.jsonl` there is unambiguously *that* agent's session.

## 4. Auto-subject

### 4.1 Summarizer (`lifecycle`)
`Summarize(ctx, sess *store.Session) (string, error)`:
1. `recentActivity(ctx, sess)`:
   - **Transcript:** `claudeProjectDir(workdir)` = `<CLAUDE_PROJECTS_DIR>/<encoded>` where `encoded` replaces `/` with `-` in the absolute workdir (matching Claude Code's scheme). Pick the newest `*.jsonl`, read the last few entries, extract their text content.
   - **Fallback:** if the project dir/file is missing or empty, use the tmux pane (`capture-pane -p -t <id>`, last ~40 lines) via the `Runner`.
2. If both are empty, use the seed prompt.
3. `out := run("claude", "-p", summaryArg(text))` where `summaryArg` = an instruction asking for a ≤8-word phrase describing what the agent is working on, + the text.
4. `parseSummary(out)`: trim, collapse whitespace/newlines, strip surrounding quotes, cap to ~80 chars.

Seams for testing: `CLAUDE_PROJECTS_DIR` (config, default `~/.claude/projects`) lets tests point the transcript root at a temp dir; the `claude -p` call and pane capture go through the existing `Runner` (fakeable); `parseSummary` and `claudeProjectDir` are pure.

### 4.2 Seed
`lifecycle.Spawn` sets `sess.Subject` to a truncated form of the prompt (first ~10 words) for prompt agents — instant, no Claude call, never blank. Typed agents start with an empty subject (filled by the first poller refresh).

### 4.3 Refresh (poller, throttled)
The poller drives refresh (it already ticks and detects pane changes):
- `Deps` gains `Summarize(ctx, *store.Session) (string, error)` and `UpdateSubject(ctx, id, subject string) error`.
- `Poller` gains `summarizeAfter time.Duration` and an in-memory `lastSummary map[string]time.Time`.
- In `tick`, for an alive, non-terminal session: if the pane changed this tick **and** `now - lastSummary[id] >= summarizeAfter`, call `Summarize` → `UpdateSubject` → record `lastSummary[id]` and mark the tick `changed` (so `OnChange`/`notify` fires → SSE push). Throttling bounds `claude -p` to ≤ once per `summarizeAfter` per active agent.
- Errors from `Summarize` are logged and skipped (never block the tick).

## 5. Components

- **store** (`internal/store`): `Session` gains `Workdir string` (`bson/json:"workdir"`) and `Subject string` (`bson/json:"subject"`); add `UpdateSubject(ctx, id, subject string) error` (mirrors `UpdateType`).
- **config** (`internal/config`): add `ClaudeProjectsDir` (`CLAUDE_PROJECTS_DIR`, default `<home>/.claude/projects`).
- **lifecycle** (`internal/lifecycle`): prompt-mode `Spawn` → per-agent `mkdir`+workdir, set `sess.Workdir`, seed `sess.Subject`; typed-mode sets `sess.Workdir`; add `Summarize` + `recentActivity` + `claudeProjectDir` + `summaryArg` + `parseSummary`. The `Lifecycle` struct gains a `projectsDir` field (from config) for transcript lookup.
- **poller** (`internal/poller`): `Deps` += `Summarize`/`UpdateSubject`; `Poller` += `summarizeAfter` + `lastSummary`; throttled refresh in `tick`. `New` keeps its signature; add a setter or constructor param for `summarizeAfter` (e.g. `New(d, stuckAfter, summarizeAfter)`).
- **daemon** (`internal/daemon`): `pollerDeps` implements `Summarize` (delegates to `lifecycle.Summarize`) and `UpdateSubject` (store); the daemon's `NewPollerDeps` gains access to the `*lifecycle.Lifecycle` (not just the `Runner`) so it can call `Summarize`; wire `summarizeAfter` into `poller.New`.
- **cli** (`internal/cli`): `ls` adds `DIR` (basename of workdir) + `SUBJECT` columns (SUBJECT replaces the old DETAIL); `status` prints full `workdir` + `subject`.
- **web** (`web/`): `types.ts` `Session` += `workdir`/`subject`; `AgentList` shows subject; `AgentDetail` shows workdir + subject.

## 6. Error handling
- No transcript / unreadable → pane fallback → seed prompt; summarizer never hard-fails a tick.
- `claude -p` unavailable/errors → keep the previous subject (or the seed); logged.
- `mkdir` failure in prompt-spawn → spawn returns the error (the agent can't start without a workdir).
- Shared-cwd agents (multiple no-worktree agents in the same repo) → transcript lookup may pick a sibling session; acceptable for v1, pane fallback covers the common case. Noted.

## 7. Testing
- **store:** `UpdateSubject` round-trip + `Workdir`/`Subject` persist (testcontainers / BSON).
- **config:** `ClaudeProjectsDir` default + env override.
- **lifecycle:** `parseSummary` table; `claudeProjectDir` encoding (`/Users/x/agentctl-agents/agent-a1b2` → `<root>/-Users-x-agentctl-agents-agent-a1b2`); `recentActivity` reads newest `.jsonl` from a temp projects dir and falls back to pane (FakeRunner) when absent; `Summarize` end-to-end (FakeRunner `claude -p` + temp transcript / pane); prompt-mode `Spawn` creates `<base>/<id>` (`mkdir -p` argv), sets `Workdir` + seeded `Subject`.
- **poller:** refresh fires only when due (interval elapsed) AND pane changed; not when throttled; `Summarize` error is swallowed.
- **cli/web:** `ls` shows DIR+SUBJECT; types compile; GUI builds.

## 8. Out of scope (this sub-project)
- The TUI (sub-project A) and the Claude skill (sub-project C).
- Re-mapping a transcript when several agents share one cwd (best-effort + pane fallback).
- Summarizing on every tick (throttled by design).
