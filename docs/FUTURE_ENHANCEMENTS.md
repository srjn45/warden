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

#### 20. Agent grouping/filtering — *shipped*
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

#### 42. Interactive tutorial — *shipped*
Ships as **`wd tutorial`**: a friendly, idempotent walkthrough of the core loop
(spawn → watch → talk → tear down) plus pointers to the cockpit TUI and web GUI.
Completing it (or `wd tutorial --skip`) writes a `tutorial-complete` marker in
`<data_dir>`; `--reset` clears it to run fresh. A **non-blocking** first-run hint
(one line to stderr) points new operators at the command, gated on a missing
marker **and** an interactive TTY **and** the `tutorial` config setting (default
on) — never shown for piped/non-TTY output or the daemon/MCP/full-screen
surfaces, so automation is untouched. Thin CLI verb with pure, unit-tested
helpers (marker read/write/reset, step list, suppression logic); no daemon
change. See FEATURES.md §24 and
`docs/superpowers/specs/2026-06-25-warden-interactive-tutorial-design.md`.
_Originally framed "Necessity: low for a single-author tool" — shipped anyway as
cheap, self-contained onboarding polish that stays out of the way._

#### 43. API documentation (OpenAPI) — *shipped*
A machine-readable **OpenAPI 3.x** description of the daemon's REST API plus an
interactive **Swagger UI at `/api/docs`** (raw spec at `/api/docs/openapi.yaml`;
FEATURES.md §27). The spec is **derived from the real route table** — a drift-guard
test (`apidocs_routes_test.go`) walks the live chi mux and asserts two-way equality
between the registered routes and the spec's paths, so an undocumented endpoint (or
a stale spec entry) fails CI. Schemas are modelled off the actual Go types
(`store.Session`, the daemon request DTOs, `lifecycle.*Result`, `snapshot.Snapshot`,
`pipeline.Pipeline`, …). Served from a self-contained `internal/daemon/apidocs`
package that **embeds** the spec and a **pinned, vendored** `swagger-ui-dist@5.17.14`
(no runtime CDN — works offline and in the container), reusing the daemon's
`go:embed`+handler pattern. The docs surface is **public** (like `/healthz` and the
static SPA shell — the spec holds no secrets), while still documenting the
`bearerAuth` scheme that gates every data/action route. Gated by the `api_docs`
config setting (default on). See
`docs/superpowers/specs/2026-06-25-warden-openapi-api-docs-design.md`.

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

#### 46. Snapshot/checkpoint system — *shipped*
Checkpoint an agent's **worktree state + session transcript** at a known-good point
and roll back to it, shipped as **`wd snapshot create/list/restore`** (CLI + MCP
`snapshot_create`/`snapshot_list`/`snapshot_restore`; FEATURES.md §23). Capture is
**non-destructive** — `git stash create` builds a commit object recording the
working tree without touching it — and records HEAD/branch/dirty-files plus the
tmux-pane transcript to a JSON store under `<data_dir>/snapshots/`. Restore
re-applies the stash onto the recorded worktree with the lifecycle rails (refuses a
dirty tree unless `--force`, never `main`/`master`) and is reversible-safe (stash
*apply* keeps the snapshot usable; conflicts are handed back like `wd sync`).
Self-contained `internal/snapshot` package (pure helpers + a runner over the shared
`lifecycle.Runner` seam), gated by the `snapshots` config setting (default on). See
`docs/superpowers/specs/2026-06-25-warden-snapshot-checkpoint-design.md`.

#### 47. Plugin system — *shipped*
Extend warden with **custom agent task types** and **lifecycle hooks** without
forking, shipped as a thin MVP. Plugins are **external executables** registered in
config (`plugins` gate + `plugin_registry`) and invoked over a documented,
versioned **JSON-over-stdio protocol** — request on stdin, response on stdout,
hard `CommandContext` timeout — deliberately mirroring warden's existing
PreToolUse guard hooks (`warden hook guard`/`git-guard`/`check-guard`) rather than
the fragile Go `plugin` package or a heavy WASM runtime (decision + rationale in
the spec). Hooks fire at the spawn/commit/check lifecycle points (event set:
pre/post-spawn, -commit, -check, plus a reserved pre-teardown) and are **advisory
and fail-open** — a broken/slow/missing plugin is logged and skipped, never
blocking or crashing an agent. Custom task types slot into the closed `store.Type`
enum via a function-var seam (`store.SetCustomTypeLookup`) so `Valid` /
`DefaultWorktree` / `NormalizeType` recognize plugin types **without touching any
built-in's behavior**; each custom type declares its own worktree isolation
policy. **Off by default** (plugins run external code). Self-contained
`internal/plugin` package (`protocol`/`registry`/`dispatcher`), `wd plugin list`
CLI, one example plugin under `examples/plugins/`. See
`docs/superpowers/specs/2026-06-25-warden-plugin-system-design.md` and FEATURES.md
§26.

#### 48. AI-powered insights — *shipped*
Mine warden's **own history** — completed and active agent sessions plus recorded
resource metrics — into actionable suggestions, shipped as **`wd insights`** (CLI +
MCP `insights`; FEATURES.md §25). A **deterministic statistics core** (durations by
type with median/p90/max and outlier flags, frequently co-edited files, error rates by
type, busiest hours, and sequential-but-disjoint sessions that could have run in
**parallel**) runs with **no LLM**; an **optional local-LLM narration layer** (the
`Narrate` seam over `llm.Completer`) summarizes the report when `local_llm` is enabled
and **degrades gracefully** to the deterministic text on any model error/empty reply —
mirroring the digest narrator. The parallelization suggester only fires on finished,
same-repo sessions whose run windows do **not** overlap and whose edited file sets are
**disjoint**. Self-contained, fully unit-tested `internal/insights` package (pure
aggregation + suggester + narrator) behind a shared `client.Insights` aggregator, gated
by the `insights` config setting (default on). See
`docs/superpowers/specs/2026-06-25-warden-ai-powered-insights-design.md`.

---

## 📊 Priority Matrix (reassessed 2026-06-25)

Re-scored on **feasibility × necessity** for what warden actually is today: a
solo-operator tool for orchestrating Claude Code agents, with remote access (the
flagship), mature pipelines, structured logging, the collab MVP, the **full
orchestration brain (#49 — isolation/git/check enforcement + local-LLM provider; see
[FEATURES.md §21–22](FEATURES.md#21-local-llm-provider-internalllm))**, and the
**local-LLM orchestrator (#50, `wd orch`)** all shipped. With the north-star orchestrator now landed, weight shifts toward **dev-loop
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
10. Tag system (#30, 3-4 h) · Agent grouping/filtering (#20, shipped) — pair well with search.
12. Export/import sessions (#7, 2 h) — necessity dropped: the store is already a
    single dir you can copy; real value is only selective/portable export.
14. Docker/container support (#41, 2 days) — the one "platform" item with real pull,
    since remote access makes containerized deployment plausible. **N: low-medium.**

### 🔮 Tier 4 — Future / large bets (foundation- or usage-gated)
15. Finish inter-agent collaboration (#44, 1-2 weeks) — correctly deferred behind
    real usage; MVP already covers file-conflict detection.
16. ~~Snapshot/checkpoint (#46)~~ — **shipped** (`wd snapshot`, FEATURES.md §23). ·
    ~~OpenAPI docs (#43)~~ — **shipped** (`/api/docs` Swagger UI, FEATURES.md §27).

### 🧊 Tier 5 — Parked (necessity too low for a solo tool; don't build speculatively)
Reassessed *downward* — keep on the list for completeness, but these need a concrete
demand signal before they're worth the effort:
- **Distributed warden** (#14) & **Multi-user support** (#31) — enterprise/multi-tenant;
  no second user or second machine in play.
- **Windows support** (#40) — user runs Linux; tmux dependency makes this WSL-only anyway.
- **Jira integration** (#33) — user's loop is GitHub, not Jira.
- ~~**Plugin system** (#47)~~ — **shipped** as a thin MVP (custom task types +
  lifecycle hooks via external executables over JSON-over-stdio; `wd plugin`,
  FEATURES.md §26). Built ahead of a driving use case as a deliberately minimal,
  default-off, fail-open extension seam — not the speculative full runtime.
- ~~**Interactive tutorial** (#42)~~ — **shipped** (`wd tutorial`, FEATURES.md §24);
  built anyway as cheap, out-of-the-way onboarding polish.
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
