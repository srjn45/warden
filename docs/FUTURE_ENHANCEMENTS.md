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

#### 3. Pipeline templates — *not started*
**Effort:** 2 hours
**Value:** Faster pipeline authoring

Ship 3-4 common pipeline templates.

```bash
warden pipeline create --template analyze-implement-review
warden pipeline list-templates
```

**Templates:** `analyze-implement-review`, `parallel-tasks`, `test-fix-verify`,
`research-synthesis`.

**Implementation:** embed via `go:embed` in `internal/pipeline/`; `--template`
renders with placeholder substitution. (Pairs with the shipped `pipeline validate`.)

---

#### 5. Web dashboard keyboard shortcuts — *not started*
**Effort:** 2 hours
**Value:** Power-user productivity

`?` help overlay, `n` new agent, `/` focus filter, `r` refresh, `Esc` close,
`1-9` tab switch, `j/k` list nav. (Individual modals already handle local keydown;
there is no global shortcut layer yet.)

---

#### 7. Export/Import sessions — *not started*
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

#### 8. Improve CLI test coverage to 50%+ — *ongoing*
**Effort:** 3-4 hours

CLI remains the lowest-covered package. Table-driven flag parsing, error paths,
output formatting, mocked daemon responses.

---

## 📊 Observability & Monitoring

#### 11. Agent performance history — *partial*
**Effort:** 4-6 hours

Live resource sampling exists via the metrics system (see FEATURES.md §11); what's
missing is persisted per-agent *history* (runtime, turn count, files modified,
context trend) and an anomaly warning surface.

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

## 🔄 Auto-Restart & Resilience

#### 17. Crash detection improvements — *partial*
**Effort:** 4 hours

Stuck-state reclassification (`stuckAfter`) and crash-exit-code recording exist.
Remaining: OOM-kill heuristics, "suggest `/compact` before crash," explicit
infinite-loop detection beyond the stuck timer.

---

## 🎨 Web UI Enhancements

#### 19. Dark mode toggle — *partial (scaffold only)*
**Effort:** ~1.5 h remaining. `app.css` already declares `color-scheme: light dark`
and ships `prefers-color-scheme` brand assets, so the OS-driven path half-works.
Remaining: CSS custom-property theming, a LocalStorage override, and a header toggle.

#### 20. Agent grouping/filtering — *not started*
**Effort:** 4 hours. Group by type/status/tag, collapsible groups, saved presets.
(`AgentGrid.tsx` exists but renders a flat grid.)

#### 21. Batch operations — *not started*
**Effort:** 3 hours. Multi-select + bulk terminate/delete/message.

---

## 🤖 Model Selection & Configuration

#### 23. Agent templates/presets — *not started*
**Effort:** 3 hours
Save common spawn configs (`~/.warden/presets.yaml`):
```bash
warden preset save code-review --type pr-review --model opus --supervised
warden start --preset code-review --pr 1234
```

---

## 📱 Pipeline Enhancements

#### 25. Pipeline MCP tools — *not started*
**Effort:** 4-6 hours
Pipelines are still CLI-only. Add `create_pipeline` / `start_pipeline` /
`show_pipeline` / `cancel_pipeline` to `internal/mcp/server.go` (which currently
exposes agent, ctx, mailbox, and approval tools — no pipeline tools). Lets an
orchestrator Claude session drive pipelines without shelling out.

---

## 🔍 Search & Discovery

#### 28. Full-text search — *not started*
**Effort:** 6-8 hours. In-memory search across subject/prompt/type/name/pane;
`warden search`, web search bar.

#### 29. Agent history/archive viewer — *not started*
**Effort:** 4 hours. Browse/search the `closed/` store (which already persists
archived records). `warden history [--since] [--type]`; web Archive tab.

#### 30. Tag system — *not started*
**Effort:** 3-4 hours. `Tags []string` on `Session`; `--tags`; filter/search by tag.
Pairs with grouping (#20) and search (#28).

---

## 🔐 Security & Permissions

#### 31. Multi-user support — *not started* (complex)
**Effort:** 2-3 days. Per-user isolation, ACLs, shared pipelines (opt-in).
**Necessity: low for a solo tool — parked.**

#### 32. Audit log — *not started*
**Effort:** 4 hours. `~/.warden/audit.jsonl`; who/what/when/where; `warden audit log`.

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

#### 41. Docker/container support — *not started*
**Effort:** 2 days. Daemon Dockerfile, `~/.warden` volume, compose example.
The one "platform" item with real pull, since remote access makes containerized
deployment plausible.

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
condensation · 1c commit messages). Remaining LLM work moves to the orchestrator track (#50).

**Design spec:** [`docs/superpowers/specs/2026-06-24-warden-orchestration-brain-design.md`](superpowers/specs/2026-06-24-warden-orchestration-brain-design.md).

#### 50. Orchestrator — local-LLM conductor (thin-translator) — *not started (designed; phased A→D)*
**Effort:** Phase A ~2 days · Phase B ~3 days · Phase C ~1 day · Phase D ~2 days
**Value:** Cut operator friction on multi-step orchestration without spending Claude tokens —
a warden-aware local-LLM REPL that turns natural-language intent into **confirmed** warden
tool calls (spawn/monitor/teardown agents, drive pipelines, run the git/check lifecycle).
**It conducts; it never implements** — there is no edit/write/bash tool in its registry, so
code work is always delegated by `spawn_agent`-ing a Claude agent.

Builds directly on #49 (the `internal/llm` provider seam + the git/check tools it routes
through). A second front-end onto the same daemon client the MCP server uses — no new
business logic.

- **Phase A** — additive `Chatter` (multi-turn, tool-calling) seam in `internal/llm`,
  backed by Ollama `/api/chat`; reuses the `local_llm*` config. Same tiny-client discipline
  (non-streaming, hard timeout, byte cap, error-so-caller-bails) plus a reliability floor for
  imperfect 7B tool-calling (malformed args / unknown tool / prose-instead-of-JSON → bounded
  retries, never a garbled execution). *Only net-new infrastructure.*
- **Phase B** — `internal/orchestrator` package + `warden orchestrator` (`wd orch`) REPL:
  the tool-calling loop, the registry split by side-effect (read-only auto-executes; mutating
  calls hit a **mandatory, non-config-gated confirm gate**), and capability-tier routing
  (pre-classify → plan locally / escalate one planning step to headless Claude / degrade to
  the operator — execution stays token-free warden calls). First shippable, standalone-runnable
  milestone.
- **Phase C** — cockpit master pane hosts the orchestrator-over-shell: one `buildCockpit`
  pane-command change (`self + " orchestrator"` instead of bare `$SHELL`); a `!`-prefixed line
  passes through to a persistent embedded `$SHELL` (cwd/env persist, spawn-dir semantics
  preserved), MVP non-interactive only; raw-`$SHELL` escape hatch one keypress away.
- **Phase D** — monitoring verbs (fleet summarize / triage / cleanup) as read-only registry
  calls + a local summarization pass reusing #49's `Summarize` routing.

Cross-cutting: **hardware-aware model recommendation** (`wd doctor` detects VRAM/RAM and
*recommends* — never silently swaps — a `local_llm_model`) and a model→capability-tier table
so an under-capable model degrades instead of shipping a confident-wrong plan. New config
(global policy only): `orchestrator` (default off — initial pane face), `local_llm_escalate`
(default on), `local_llm_tier` (default auto).

**Design spec:** [`docs/superpowers/specs/2026-06-25-warden-orchestrator-design.md`](superpowers/specs/2026-06-25-warden-orchestrator-design.md)
· phase plans in [`docs/superpowers/plans/`](superpowers/plans/) (`2026-06-25-warden-orchestrator-phase-{a,b,c,d}.md`).

---

## 📊 Priority Matrix (reassessed 2026-06-25)

Re-scored on **feasibility × necessity** for what warden actually is today: a
solo-operator tool for orchestrating Claude Code agents, with remote access (the
flagship), mature pipelines, structured logging, the collab MVP, and the **full
orchestration brain (#49, Phase 0 + Phase 1)** all shipped. That shifts weight toward
**spending the brain's groundwork on the operator surface (#50)** and **dev-loop
closure**, and away from **enterprise/multi-user** features whose necessity is low for
a single user.

### 🔥 Tier 1 — Do First (high necessity, low/medium effort, all feasible now)
0. **Orchestrator — local-LLM conductor** (#50, Phase A→B ~5 days to first usable) — the
   north-star follow-on now that #49 is complete: spend the `internal/llm` seam + git/check
   tools on NL→confirmed-tool-call composition (`wd orch`), confirm-before-execute so a 7B
   model is safe. Phase A (`Chatter` seam) is the only net-new infra; B ships standalone.
   **F: medium · N: high.**
1. **Pipeline MCP tools** (#25, 4-6 h) — pipelines are mature but CLI-only, so an
   orchestrator agent can't drive them. `internal/mcp/server.go` already exposes
   agent/ctx/mailbox/approval tools — adding pipeline tools is mechanical. **F: medium · N: high.**
2. **Pipeline templates** (#3, 2 h) — `go:embed` 3-4 starters; lowers the authoring
   barrier that `validate` only half-addresses. **F: easy · N: medium.**

*(#34 Slack/webhook notifications — ✅ shipped, see FEATURES.md "Webhook / Slack notifications" + the `webhook_enabled`/`webhook_url` settings.)*
*(#35 GitHub PR auto-create on done — ✅ shipped, see FEATURES.md `done --create-pr`.)*

### ⭐ Tier 2 — Do Next (solid value, mostly fleet-management)
5. **Batch operations** (#21, 3 h) — multi-select bulk terminate/delete/message;
   the most-felt gap once a fleet grows. **F: medium · N: medium.**
6. **Full-text search + history/archive viewer** (#28 + #29, ~1.5 days) — the
   `closed/` store already persists records; add `warden search` / `warden history`
   and a web Archive tab. **F: medium · N: medium.**
7. **Scheduled agents/tasks** (#15, 1-2 days) — decision doc + `robfig/cron`.
   Necessity nudged *down*: the Claude Code harness now offers external cron/schedule,
   so in-daemon scheduling is convenience, not a blocker. **F: medium · N: medium.**
8. **Finish crash detection + agent performance history** (#17 + #11, ~1.5 days)
   — both already *partial*; complete OOM/loop heuristics and persisted per-agent
   history. **F: medium · N: medium.**
9. **Improve CLI test coverage to 50%+** (#8, ongoing) — lowest-covered package.
   **F: easy · N: medium (quality).**

### 🎯 Tier 3 — Nice to Have (cosmetic or niche; do opportunistically)
10. Tag system (#30, 3-4 h) · Agent grouping/filtering (#20, 4 h) — pair well with search.
11. Agent presets (#23, 3 h) — quality-of-life for repeated spawn configs.
12. Export/import sessions (#7, 2 h) — necessity dropped: the store is already a
    single dir you can copy; real value is only selective/portable export.
13. Finish dark-mode toggle (#19, ~1.5 h) · Web keyboard shortcuts (#5, 2 h) — polish.
14. Docker/container support (#41, 2 days) — the one "platform" item with real pull,
    since remote access makes containerized deployment plausible. **N: low-medium.**
15. Audit log (#32, 4 h) — `~/.warden/audit.jsonl`; useful once actions multiply.

### 🔮 Tier 4 — Future / large bets (foundation- or usage-gated)
16. Finish inter-agent collaboration (#44, 1-2 weeks) — correctly deferred behind
    real usage; MVP already covers file-conflict detection.
17. Snapshot/checkpoint (#46, 2 days) · OpenAPI docs (#43, 4 h) — revisit when the
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

Tier 1 first (each a self-contained win), then Tier 2 as fleet size grows:

0. **Orchestrator Phase A→B** (#50, ~5 days) — `Chatter` seam + `wd orch` REPL with the confirm gate; spends the now-complete brain groundwork on NL multi-agent composition
1. **Slack/webhook notifications** (3-4 h) — remote awareness; pairs with the now-shipped remote access
2. **Pipeline MCP tools** (4-6 h) — let orchestrator agents drive pipelines
3. **Pipeline templates** (2 h) — lowers the barrier `validate` only half-fixed
5. **Batch operations** (3 h) — first real pain point as the fleet grows
6. **Full-text search + history viewer** (~1.5 days) — manage larger/older fleets
7. **Scheduled agents/tasks** (1-2 days) — recurring automation (convenience-tier now)
8. **Finish crash detection + perf history** (~1.5 days) — complete the partials
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
