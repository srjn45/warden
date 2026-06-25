# Warden Future Enhancements & Feature Roadmap

**Last Updated:** 2026-06-25
**Current Version:** v5.1.1

This document tracks **pending** improvements and new features for warden, organized
by category and priority. Each item includes effort estimates and implementation
notes.

> **What lives where:** shipped capabilities are documented in
> [FEATURES.md](FEATURES.md) (the "what exists" catalog) — they are *not* repeated
> here. This file is forward-looking only.
>
> **Maintenance note:** This file is verified against the codebase, not just
> appended to. Before adding something as "future," grep `internal/` first —
> several items were already shipped while the roadmap still listed them as
> pending. When you finish a feature, **document it in FEATURES.md and delete its
> entry here**, so the roadmap stays a pure to-do list.
>
> **Item numbers are stable IDs** (referenced from commits/issues), so removing a
> finished item leaves a gap rather than renumbering the rest. That's intentional.

---

## 🎯 Quick Wins (1-4 hours each)

### CLI & UX Improvements

#### 7. Export/Import sessions — *shipped*
**Effort:** 2 hours
**Value:** Backup, sharing, migration

```bash
warden export --all > all-sessions.json
warden import < backup.json
```

Serialize/insert `Session` structs (metadata only; does not recreate worktrees).
Necessity is modest — the store is already a single copyable directory — so the
real value is *selective/portable* export, not whole-store backup.

---

#### 8. Improve CLI test coverage to 50%+ — *done (60.1%)*
**Effort:** 3-4 hours

Goal met: `internal/cli` rose from 48.1% to 60.1% via table-driven tests for flag
parsing, error paths, output formatting, and httptest-stubbed daemon responses
(sessions, stats, worktree/prune, collab). Further coverage is welcome but no
longer a roadmap blocker.

---

## 🌐 Remote Access & Multi-Device

#### 14. Distributed warden (multi-machine) ⭐ ENTERPRISE — *not started*
**Effort:** 1-2 weeks

Central control plane aggregating multiple daemons; route/spawn by machine;
unified dashboard; load balancing. New `internal/cluster` package. Builds on the
shipped remote-access auth. **Necessity: low for a solo/single-machine setup —
parked until there's a second machine in play.**

---

## ⏰ Scheduling & Automation

#### 15. Scheduled agents/tasks ⭐ AUTOMATION — *not started*
**Effort:** 1-2 days
**Value:** Unattended, recurring runs

**Decision doc:** `docs/superpowers/specs/2026-06-10-warden-scheduled-pipelines-decision.md`

```bash
warden schedule create "Review pending PRs" --cron "0 9 * * *" --type pr-review
warden schedule create "Deploy" --at "2026-06-15 14:00"
warden schedule list / delete <id>
```

Store in `~/.warden/schedules.json`; daemon scheduler loop (check each minute);
`github.com/robfig/cron`. Note the existing "cron" hits in `internal/` are
rate-limit reset-time parsing, not a scheduler. **Necessity nudged down** — the
Claude Code harness now offers external cron/scheduling, so in-daemon scheduling is
convenience rather than a blocker.

---

## 🎨 Web UI Enhancements

#### 20. Agent grouping/filtering — *not started*
**Effort:** 4 hours. Group by type/status/tag, collapsible groups, saved presets.
(`AgentGrid.tsx` exists but renders a flat grid.)

---

## 🔍 Search & Discovery

#### 30. Tag system — *shipped*
**Effort:** 3-4 hours. `Tags []string` on `Session`; `--tags`; filter/search by tag.
Pairs with grouping (#20) and the shipped search (#28, see FEATURES.md §14).

---

## 🔐 Security & Permissions

#### 31. Multi-user support — *not started* (complex)
**Effort:** 2-3 days. Per-user isolation, ACLs, shared pipelines (opt-in).
**Necessity: low for a solo tool — parked.**

#### 32. Audit log — *shipped*
The daemon appends an append-only action trail to `~/.warden/audit.jsonl` (one
JSON object per line, stable schema: who/what/when + target/detail). It records
the meaningful actions — spawn, terminate, delete, approve, pipeline start/cancel
— best-effort, so a write failure never blocks or fails the action. Read and
filter it with `warden audit log` (`--tail`, `--action`, `--target`,
`--since`/`--until`, `--json`); the command reads the file directly, so it works
even while the daemon is down.

---

## 📦 Integrations

#### 33. Jira integration — *not started*
**Effort:** 1 day. Auto-fetch ticket summary on spawn; post digest on completion.
**Necessity: low — the project's loop is GitHub, not Jira. Parked until needed.**

---

## ⚡ Performance & Scalability

#### 36. Goroutine-based concurrency — *partial*
**Effort:** 3-5 days remaining
The poller already runs background workers (approval worker, summarizer workers
draining off the tick loop, `wg`-tracked shutdown). Remaining: parallel batch
operations (bulk terminate/delete/status), worker-pool for resource-intensive ops,
parallel independent-job execution in the pipeline executor, load testing with
100+ agents. **Only matters past ~100 concurrent agents — not the current scale.**

---

## 🌍 Platform Support

#### 40. Windows support — *not started*
**Effort:** 2-3 days (WSL2 for tmux). Service install + path handling.
**Necessity: low (primary user on Linux; tmux makes this WSL-only anyway).**

---

## 📚 Documentation & Onboarding

#### 42. Interactive tutorial — *not started*
**Effort:** 1 day. First-run guided walkthrough (detect `~/.warden/tutorial-complete`).
**Necessity: low for a single-author tool.**

#### 43. API documentation (OpenAPI) — *not started*
**Effort:** 4 hours. Generate `openapi.yaml`; serve Swagger UI at `/api/docs`.
Revisit when the remote API gains outside consumers.

---

## 🚀 Advanced Features

#### 44. Intelligent inter-agent collaboration ⭐ NEXT-GEN — *MVP shipped; advanced deferred*
**Design:** `docs/superpowers/specs/2026-06-14-intelligent-inter-agent-collaboration-design.md`

The file-conflict-detection MVP (shared context, mailbox, detection engine, web
card, conflict-check prompt hint) is **shipped — see FEATURES.md §6**.

FSNotify real-time detection is now **shipped** (see FEATURES.md §6): an
fsnotify watcher gives subsecond reaction with watch-set reconciliation against
the active-session view and an inotify watch budget, falling back to the poll
loop when unavailable.

**Still deferred behind real usage** (see the spec's Appendix A):
work-overlap/dedup detection (OverlapDetector — its plan-file signal is dead
under current naming and needs redesign), GitHub branch/CI tracking
(BranchTracker), collaboration groups, SSE replay + multi-cache layer.

#### 45. Agent chaining/handoff — *shipped*
Same-workspace succession ships as `warden rotate` (retire + same-worktree successor,
FEATURES.md §7). The cross-agent half now ships as **`warden handoff`**: an agent (or
the operator) delegates a sub-task to a **different** agent and keeps running — default
mode spawns a fresh delegate in its own isolated worktree, `--to <id>` delivers into an
existing agent's inbox. Handoff content is inlined into the recipient's prompt/message
(it runs in a different worktree). Thin CLI verb over existing client methods + a
skill-driven review gate; no daemon change. See FEATURES.md §7 and
`docs/superpowers/specs/2026-06-25-warden-handoff-design.md`.

#### 46. Snapshot/checkpoint system — *not started*
**Effort:** 2 days. Checkpoint worktree (git stash) + transcript; restore.

#### 47. Plugin system — *not started*
**Effort:** 3-4 days. Custom task types + lifecycle hooks (Go plugin or WASM).
**Speculative — no driving use case yet.**

#### 48. AI-powered insights — *not started*
**Effort:** 2-3 days. Analyze historical patterns; suggest parallelization / hints.
**Speculative — no driving use case yet.**

---

## 🧠 Orchestration & Token Reduction

#### 49. Orchestration brain — responsibility transfer + enforcement + local LLM — *Phase 0 + Phase 1 complete*
**Effort:** Phase 0a ~1 day · 0b ~2 days · 0c ~1 day · Phase 1 ~3-4 days (incremental)
**Value:** Cut Claude token spend; enforce worktree isolation; retire the operator's
manual git lifecycle.

Move deterministic responsibilities off Claude agents onto warden, and **enforce** the
boundary with a PreToolUse hook (steer via system prompt → deny+redirect hook → restrict
via `disallowedTools`). Most of the win needs **no LLM**; an optional local model (Ollama)
handles the fuzzy-cheap middle (classification, log summarization, headless commit
messages), proven first by swapping the existing headless-Claude `Classify` call.

- **0a Isolation enforcement** — ✅ **shipped.** **0a-1:** default-isolate every
  write-agent (worktree unless `--in-repo`; pr-review exempt). **0a-2:** PreToolUse guard
  that denies an isolated agent's Edit/Write escaping its worktree into the shared repo —
  built on a net-new per-agent `claude --settings` hook-delivery mechanism (`warden hook
  guard` → `POST /hooks/guard`), gated by `isolation_guard` (default on), fails open.
  Fixes the parallel-agent collision pain. *No LLM.*
- **0b Git lifecycle** — **0b-1:** ✅ **shipped** — `wd commit` / `wd push` / `wd sync` as
  CLI + MCP tools (`mcp__warden__commit`/`push`/`sync`) on the existing `lifecycle.go`
  runner, returning compact structs in place of git tool-spam. Rails (no main/master,
  no dirty-tree sync, pre-commit-failure-as-result), per-agent workdir pinning + commit
  bookkeeping, sync leaves conflicts in progress with only the conflicting files. Plus the
  Layer-1 `git_conventions` prompt steer (default on). **0b-2:** ✅ **shipped** — a second
  `PreToolUse` hook over `Bash` (`warden hook git-guard`) in the same 0a-2 per-agent
  `--settings` file that quote-aware argv-parses each command and deny-redirects raw `git
  commit|push|pull|rebase` to the warden tools (reads stay allowed), the deny message naming
  the exact replacement; static verdict (no daemon round-trip), fails open, gated by
  `git_redirect` (default on). *No LLM.*
- **0c `wd check`** — **0c-1:** ✅ **shipped** — `wd check [name]` as CLI + MCP tool
  (`mcp__warden__check`) backed by `lifecycle.Check`, running the per-project
  `.warden/check.yml` command(s) and returning pass/fail with output for only the failing
  checks (tail-truncated). Per-entry `dir:` for monorepos; config is the single source of
  truth; no-config / unknown-name return friendly errors; daemon pins to the agent's worktree
  (shared `pinnedWorkdir`) + records a `check` event; Layer-1 steer extended. **0c-2:** ✅
  **shipped** — a third `PreToolUse` Bash hook (`warden hook check-guard`) on the same
  per-agent `--settings` file deny-redirects a raw test/lint/build command the project's
  `.warden/check.yml` registers to `wd check`, reusing the runner's own config parser
  (`lifecycle.CheckCommands` — single source, no drift) and matching on leading-token prefix
  (broad runs redirect, focused `-run` runs pass through); no-config repos redirect nothing,
  reads config from the agent cwd (no daemon round-trip), fails open, gated by
  `check_redirect` (default on). Biggest raw token win. *No LLM (optional summarize is a
  Phase 1 follow-up).*
- **Phase 1 Local provider** — opt-in Ollama provider; `Classify` → headless commit
  messages → log summarization. **1a:** ✅ **shipped** — new `internal/llm` package (a
  one-method `Completer` seam + a tiny non-streaming Ollama `/api/generate` client with a
  hard timeout, byte cap, and error-so-caller-falls-back contract); `lifecycle` gains an
  optional `LLM` field (nil = off) and `Classify` routes through it first, falling back to
  headless Claude (then `TypeOther`) on any error. Gated by `local_llm` (default off) +
  `local_llm_url`/`_model`/`_timeout`; the daemon builds the provider only when enabled.
  First LLM in the tree; degrades to today's behavior when off/unreachable. **1b:** ✅
  **shipped** — `Summarize` (the ≤8-word agent-activity subject) routes through the same
  seam first, falling back to headless Claude on any local error *or empty reply* (an empty
  summary carries no signal, so unlike `Classify` it is not trusted); and `lifecycle.Check`
  condenses an **oversized** failure log (output past `maxCheckOutputLines`) via the local
  model into the distinct failures, with the deterministic tail-truncation as the fallback
  (model error / empty reply / no model → the agent still gets the failure). Within-cap
  failures skip the model entirely. **1c:** ✅ **shipped** — `wd commit` / MCP `commit` no
  longer require `-m`; `lifecycle.Commit` fills a missing message in after staging via the
  same seam — (a) the author's `-m`, else (b) a Conventional-Commits subject distilled by the
  local model from the staged diff (`git diff --cached`, capped to 16 KiB of valid UTF-8),
  else (c) a deterministic conventional-commit floor derived from the changed paths. Every
  degradation (no model / error / timeout / empty reply) falls to the floor, so a blank commit
  is impossible — mirroring the Classify/Summarize fallback pattern.

**Phase 1 is now complete** (1a Classify · 1b Summarize + oversized check-failure
condensation · 1c commit messages). Its remaining LLM work shipped as the orchestrator
conductor (#50) — now complete; see [FEATURES.md §17](FEATURES.md#17-orchestrator-wd-orch).

**Design spec:** [`docs/superpowers/specs/2026-06-24-warden-orchestration-brain-design.md`](superpowers/specs/2026-06-24-warden-orchestration-brain-design.md).

---

## 📊 Priority Matrix (reassessed 2026-06-25)

Re-scored on **feasibility × necessity** for what warden actually is today: a
solo-operator tool for orchestrating Claude Code agents, with remote access (the
flagship), mature pipelines, structured logging, the collab MVP, the **full
orchestration brain (#49)**, and the **local-LLM orchestrator (#50, `wd orch`)** all
shipped. With the north-star orchestrator now landed, weight shifts toward **dev-loop
closure** and fleet-management polish, and away from **enterprise/multi-user** features
whose necessity is low for a single user.

### 🔥 Tier 1 — Do First (high necessity, low/medium effort, all feasible now)
_Cleared — the orchestrator (#50, `wd orch`) shipped; see [FEATURES.md §17](FEATURES.md#17-orchestrator-wd-orch). The items below are the new front of the queue._

### ⭐ Tier 2 — Do Next (solid value, mostly fleet-management)
7. **Scheduled agents/tasks** (#15, 1-2 days) — decision doc + `robfig/cron`.
   Necessity nudged *down*: the Claude Code harness now offers external cron/schedule,
   so in-daemon scheduling is convenience, not a blocker. **F: medium · N: medium.**
9. **Improve CLI test coverage to 50%+** (#8, done — 60.1%) — was the lowest-covered package.
   **F: easy · N: medium (quality).**

### 🎯 Tier 3 — Nice to Have (cosmetic or niche; do opportunistically)
10. Tag system (#30, 3-4 h) · Agent grouping/filtering (#20, 4 h) — pair well with search.
12. Export/import sessions (#7, 2 h) — necessity dropped: the store is already a
    single dir you can copy; real value is only selective/portable export.
14. Docker/container support (#41, 2 days) — the one "platform" item with real pull,
    since remote access makes containerized deployment plausible. **N: low-medium.**

### 🔮 Tier 4 — Future / large bets (foundation- or usage-gated)
15. Finish inter-agent collaboration (#44, 1-2 weeks) — correctly deferred behind
    real usage; MVP already covers file-conflict detection.
16. Snapshot/checkpoint (#46, 2 days) · OpenAPI docs (#43, 4 h) — revisit when the
    remote API gains outside consumers.

### 🧊 Tier 5 — Parked (necessity too low for a solo tool; don't build speculatively)
Reassessed *downward* — keep on the list for completeness, but these need a concrete
demand signal before they're worth the effort:
- **Distributed warden** (#14) & **Multi-user support** (#31) — enterprise/multi-tenant;
  no second user or second machine in play.
- **Windows support** (#40) — user runs Linux; tmux dependency makes this WSL-only anyway.
- **Jira integration** (#33) — user's loop is GitHub, not Jira.
- **Plugin system** (#47) & **AI-powered insights** (#48) — speculative; no driving use case.
- **Interactive tutorial** (#42) — onboarding ROI is ~zero for a single-author tool.
- **Goroutine batch concurrency** (#36) — only matters past ~100 concurrent agents;
  not the current scale.

---

## 🎬 Recommended Implementation Order

Now that the orchestrator (#50) has shipped, the queue leads with fleet-management
and dev-loop items:

7. **Scheduled agents/tasks** (1-2 days) — recurring automation (convenience-tier now)
9. **Finish inter-agent collaboration** (1-2 weeks) — next-gen, foundation already in

---

## 📝 Notes

- **Design specs** live in `docs/superpowers/specs/` — check for one before starting.
- **Shipped features** are catalogued in [FEATURES.md](FEATURES.md); usage in
  [USAGE.md](USAGE.md).
- Effort estimates are approximate.
- Some features are interdependent (remote access → distributed; metrics →
  performance history; search ↔ tags ↔ grouping).
- Platform-specific work (macOS/Linux) may need separate implementations.

---

## 🤝 Contributing

When implementing features from this roadmap:

1. Check for a design spec in `docs/superpowers/specs/`
2. Write tests first (TDD where possible)
3. Update docs (README, FEATURES.md, USAGE.md)
4. Run `make verify` before committing
5. **Update this file:** document the finished feature in FEATURES.md and delete its
   entry here, so the roadmap stays a pure to-do list

---

**Questions or suggestions?** Open an issue at https://github.com/srjn45/warden/issues
