# Warden Future Enhancements & Feature Roadmap

**Last Updated:** 2026-06-27
**Current Version:** v5.20.0

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

## 🌐 Remote Access & Multi-Device

#### 14. Distributed warden (multi-machine) ⭐ ENTERPRISE — *not started*
**Effort:** 1-2 weeks

Central control plane aggregating multiple daemons; route/spawn by machine;
unified dashboard; load balancing. New `internal/cluster` package. Builds on the
shipped remote-access auth. **Necessity: low for a solo/single-machine setup —
parked until there's a second machine in play.**

---

## 🔐 Security & Permissions

#### 31. Multi-user support — *not started* (complex)
**Effort:** 2-3 days. Per-user isolation, ACLs, shared pipelines (opt-in).
**Necessity: low for a solo tool — parked.**

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

## 🚀 Advanced Features

#### 44. Intelligent inter-agent collaboration ⭐ NEXT-GEN — *MVP + BranchTracker shipped; rest dropped*
**Design:** `docs/superpowers/specs/2026-06-14-intelligent-inter-agent-collaboration-design.md`

The file-conflict-detection MVP (shared context, mailbox, detection engine, web
card, conflict-check prompt hint), FSNotify real-time detection, and **GitHub
branch/CI tracking (BranchTracker)** are **shipped — see FEATURES.md §6**.

**Audited and dropped** (see the spec's Appendix A) — these were the rest of the
"advanced collaboration" bucket, and on inspection none earns its complexity at
warden's ≤10-agent scale:
- **OverlapDetector** — its only signal was an agent's plan file, which no longer
  exists under current naming; the idea overlaps the shipped file-conflict
  detector. Dead as designed; would need a fresh signal to be worth anything.
- **Collaboration groups** — redundant with the pipeline subsystem (dependencies,
  handoffs, shared context already express grouped work).
- **SSE replay + multi-cache layer** — an optimization for a load (100+ agents)
  warden doesn't carry; straight serial recomputation is correct at this scale.

---

## 🖥️ Web Cockpit / Full-Screen TUI

#### 51. Self-healing web cockpit session — *not started*
**Effort:** 0.5–1 day

`EnsureWebCockpit` (`internal/tui/compositor.go`) is idempotent on a bare
`tmux has-session` probe: if a `warden-web-cockpit` session exists, it's reused
as-is, with no check that it's *healthy* (3 panes; top-left actually running
`warden tui --pane=list`). A session left in a degraded shape — e.g. the
pre-#151 build where `q` dropped the list pane to a bare shell, or a daemon crash
mid-`buildCockpit` — is then handed to every client forever, and survives daemon
restarts/reinstalls because it lives in the **tmux server, not the daemon**. The
only recovery today is a manual `tmux kill-session -t warden-web-cockpit`.

This is **not a recurrence risk under normal use** — #151's `q` now runs
`killCockpitCmd` and tears the whole session down cleanly — but there's no
in-product recovery path when a session *does* end up wedged.

Two options (pick one or both):
- **Validate-and-rebuild** — on attach, verify the session has the expected pane
  layout with the list pane running the bloom app; if not, kill and rebuild.
  Makes the cockpit genuinely self-healing with no user action.
- **Explicit rebuild affordance** — a `warden tui --rebuild-web-cockpit` flag /
  daemon endpoint, surfaced as a small "↻ rebuild" control on the `/tui` screen,
  so a wedged cockpit can be reset from the browser without shelling in.

**Necessity: low-moderate** — purely a robustness/recoverability gap, no
day-to-day impact now that `q` exits cleanly.

---

## 🤖 Agent Backends & Ecosystem

#### 52. Pluggable agent backends (beyond Claude Code) — *idea, not yet scoped*
**Effort:** TBD (deep-dive later)

Generalize the agent layer so warden can drive **other console-based coding
agents** (e.g. other CLI AI assistants) the same way it drives Claude Code, not
just Claude Code alone. Today the spawn/attach/lifecycle plumbing assumes the
`claude` binary; factoring that into a backend interface (launch command,
prompt/handoff protocol, idle/needs-input detection, digest parsing) would let an
operator pick a backend per agent. **Why it matters:** broadens warden's reach
beyond Claude-Code users to any developer running a terminal agent, which is the
single biggest lever on adoption. Needs a design pass on the backend abstraction
and which agents to support first.

---

## 🧠 Project Context & Memory

#### 53. `wd init` + per-project context store under `~/.warden` — *idea, not yet scoped*
**Effort:** TBD (deep-dive later)

A `wd init` command that registers a **project root** and keeps warden's
per-project state under `~/.warden/<project>/` instead of inside the repo — so
there's nothing to `.gitignore` and no warden artifacts polluting the working
tree. The store would hold the project's durable context: session history,
warden's accumulated understanding of the codebase, and a memory bank that
persists across agents and runs. **Why it matters:** gives warden (and especially
the local-LLM REPL, #50) real long-lived project memory to ground better answers
and orchestration, while keeping the repo clean. Needs a design pass on the
store's layout, what gets persisted vs. recomputed, and how it's keyed to a repo
(path vs. remote vs. an explicit id).

---

## 📊 Priority Matrix (reassessed 2026-06-26)

Re-scored on **feasibility × necessity** for what warden actually is today: a
solo-operator tool for orchestrating Claude Code agents, with remote access (the
flagship), mature pipelines, structured logging, the collab MVP, the **full
orchestration brain (#49)** and the **local-LLM REPL (#50, `wd repl`)** all
shipped. With the north-star orchestrator landed and the recent onboarding /
extensibility batch merged (#42 tutorial, #43 OpenAPI docs, #46 snapshots, #47
plugins, #48 insights — all in FEATURES.md), the remaining roadmap is mostly
**usage-gated large bets** and **parked enterprise features** whose necessity is low
for a single user.

> **Tiers 1–3 are cleared** — the REPL (#50, `wd repl`) shipped (see
> [FEATURES.md §17](FEATURES.md#17-interactive-mode--repl-wd-repl)), along with the
> onboarding / extensibility batch (#42, #43, #46, #47, #48). Only Tiers 4–5
> remain below: a few new early-stage bets (#51–#53) and the parked enterprise set.

### 🔮 Tier 4 — Future / large bets (usage-gated)
- **Self-healing web cockpit session (#51)** — small (0.5–1 day) robustness item:
  make `EnsureWebCockpit` validate/rebuild a wedged cockpit instead of reusing it
  blindly. Low-moderate necessity; no day-to-day impact now that `q` exits cleanly.
- **Pluggable agent backends (#52)** — early idea: drive console agents beyond
  Claude Code via a backend interface. Biggest adoption lever; needs a design pass.
- **`wd init` + per-project context store (#53)** — early idea: durable per-project
  memory under `~/.warden` (no repo pollution) to ground warden / the local-LLM
  REPL. Needs a design pass on store layout and repo keying.
- Inter-agent collaboration (#44) is closed: the file-conflict MVP +
  BranchTracker shipped and the rest was audited and dropped (see §44 above). A
  fresh demand signal would be needed to reopen any of it.

### 🧊 Tier 5 — Parked (necessity too low for a solo tool; don't build speculatively)
Keep on the list for completeness, but these need a concrete demand signal before
they're worth the effort:
- **Distributed warden** (#14) & **Multi-user support** (#31) — enterprise/multi-tenant;
  no second user or second machine in play.
- **Windows support** (#40) — user runs Linux; tmux dependency makes this WSL-only anyway.
- **Jira integration** (#33) — user's loop is GitHub, not Jira.
- **Goroutine batch concurrency** (#36) — only matters past ~100 concurrent agents;
  not the current scale.

---

## 🎬 Recommended Implementation Order

**The near-term queue is clear.** Everything Tier-1-through-Tier-3 has shipped,
and inter-agent collaboration (#44) is closed (MVP + BranchTracker shipped, the
rest dropped — see §44). What remains is all Tier 5 (parked): build only on a
concrete demand signal, not speculatively.

---

## 📝 Notes

- **Design specs** live in `docs/superpowers/specs/` — check for one before starting.
- **Shipped features** are catalogued in [FEATURES.md](FEATURES.md); usage in
  [USAGE.md](USAGE.md).
- Effort estimates are approximate.
- The remaining (parked) items are largely independent; the one dependency worth
  noting is that **distributed warden (#14)** builds on the already-shipped
  remote-access auth.
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