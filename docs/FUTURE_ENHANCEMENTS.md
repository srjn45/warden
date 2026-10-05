# Warden Future Enhancements & Feature Roadmap

**Last Updated:** 2026-10-05
**Audited Against:** `main` at v9.18.0 (through PR #751)

This document tracks **pending** improvements and new features for warden. Each
item includes an effort estimate and implementation notes.

> **What lives where:** shipped capabilities are documented in
> [FEATURES.md](FEATURES.md) (the "what exists" catalog) — they are *not* repeated
> here. This file is forward-looking only. The larger hub / cluster / teams
> programme is tracked separately in
> [`specs/2026-08-28-future-scope.md`](specs/2026-08-28-future-scope.md).
>
> **Maintenance note:** This file is verified against the codebase, not just
> appended to. Before adding something as "future," grep `internal/` first.
> When you finish a feature, **document it in FEATURES.md and delete its entry
> here**, so the roadmap stays a pure to-do list.
>
> **Item numbers are stable IDs** (referenced from commits, code comments and
> issues), so removing a finished item leaves a gap rather than renumbering the
> rest. That's intentional. A feature that shipped but left follow-ups keeps its
> number, and its entry lists only what is still open.

---

## 🔧 Open follow-ups on shipped features

These features are shipped and catalogued in FEATURES.md; only the listed
remainders are open.

#### 52. Pluggable agent backends — *shipped; adapter hardening remains*
**Effort:** 0.5–2 days per item, independent of each other

All eight backends ship (see `docs/agent-backends/` and the README backend
table). Remaining per-adapter gaps:

- **Exact-id resume for OpenCode and Crush.** Codex (`DiscoverSessionID`) and
  Antigravity (`DiscoverSessionIDFromLog`) pin the backend-minted session id;
  OpenCode and Crush still resume dir-scoped. Both adapters already prefer a
  pinned id when one is present, so this is wiring the discoverer only.
- **OpenCode approval parsing.** `ParseApproval` is a stub, so OpenCode's
  interactive permission prompts never reach the approvals inbox.
- **OpenCode native cost.** Warden reports tokens only for OpenCode and omits
  it from `wd savings`; OpenCode exposes first-class cost/tokens it could read.
- **Cursor structured transcript.** A parser exists and is tested against a
  fixture, but `StructuredTranscript` is off and `TranscriptPath` returns
  nothing, so Cursor agents produce no digests (Tier C).

#### 55. Fast-Brain micro-decision engine — *shipped; four remainders*
**Effort:** 0.5–1 day each

- **Automatic remediation of transient crashes.** Crash triage classifies
  `transient_error` and records a "consider switching backend" event, but
  nothing acts on it (no automatic backend switch or rebase abort).
- **Activity badge in the web UI.** The `activity` field is on the session and
  rendered in the TUI; the web UI carries it in its types but does not show it.
- **Automatic PR on job completion.** The drafted PR title/body is used only
  when a PR is requested (`agent stop --pr`, `done --create-pr`); `wd job done`
  does not open one by itself.
- **Execution profile beyond model tier.** The router picks `tier-1/2/3` only;
  there is no per-decision `fast` / `standard` / `heavy` execution profile.

#### 51. Self-healing web cockpit session — *shipped; one nicety remains*
**Effort:** 0.5 day

An in-browser "↻ rebuild" control. Today a wedged cockpit is rebuilt
automatically on attach, or explicitly with `warden tui --rebuild-web-cockpit`.

#### 53. Project memory — *shipped; one optional leftover*
**Effort:** 1 day

Per-project partitioning of `savings` / `spend` / `insights`, which are global
today. A reporting nicety; build only if the commingling is actually felt.

#### 36. Goroutine-based concurrency — *partial*
**Effort:** 3–5 days remaining

The poller already runs background workers and the pipeline executor reconciles
concurrently. Remaining: parallel bulk operations (terminate / delete / status
across many agents), a worker pool for resource-intensive operations, and load
testing with 100+ agents. **Only matters past ~100 concurrent agents — not the
current scale.**

---

## 🧊 Parked (need a concrete demand signal)

#### 14. Distributed warden (multi-machine) — *not started*
**Effort:** 1–2 weeks

Central control plane aggregating multiple daemons; route/spawn by machine;
unified dashboard. Depends on the hub transport (feature #6/#7 in the
future-scope spec). **Parked until there is a second machine in play.**

#### 31. Multi-user support — *not started*
**Effort:** 2–3 days. Per-user isolation, ACLs, opt-in shared pipelines.
**Parked — warden is a solo-operator tool today.**

#### 33. Jira integration — *not started*
**Effort:** 1 day. Auto-fetch the ticket summary on spawn; post the digest on
completion. **Parked — the project's loop is GitHub, not Jira.**

#### 40. Windows support — *not started*
**Effort:** 2–3 days. Service install and path handling; tmux makes this
WSL2-only. **Parked — no Windows user.**

#### 54. Plugin protocol v2 (gating plugins) — *explicitly deferred; likely never*
**Effort:** large (a second, harder product — fail-closed semantics + capability
model + security review)
**Context:** plugin system v1 shipped as #47 (see FEATURES.md); design in
`docs/superpowers/specs/2026-06-25-warden-plugin-system-design.md`.

The obvious sequel to #47 is a protocol v2 where plugins can **gate** — return a
decision warden honors (approve/deny a prompt, block a commit, veto a spawn,
enforce a spend cap). **Deliberately deferred, recorded here so the reasoning
isn't relitigated.** Assessed 2026-07-03:

- **No capability gap.** Every gating feature v2 would host already exists in
  core and works: auto-approve policy + circuit breaker, cost governance,
  memory curation, and the per-agent PreToolUse gates (`guard` / `git-guard` /
  `check-guard` in `internal/cli`). v2 would *relocate* working code behind a
  riskier interface, not enable anything new.
- **Gating inverts v1's cheapness.** v1 is small *because* it's fail-open (any
  failure → log and skip; a hook can never block an agent). A gating plugin
  must fail **closed**, which buys: blocking semantics, timeout policy that
  stalls agents on third-party code, typed decision payloads, a capability
  grant model, likely long-lived plugin processes, and a security review of
  "arbitrary external code in the approval path of a security product" —
  permanently, as a versioned-protocol compatibility promise.
- **Identity conflict.** Warden is safe out of the box and adds capability
  on top of an agent rather than stripping it. A gating plugin is third-party code that
  can strip (deny/block/stall); once safety logic *can* live in a plugin, the
  pressure to move the breaker there follows, and default-on safety erodes.
- **Zero demand.** v1 has no third-party ecosystem yet; no user has named a
  gating need that core config can't express.

**Revisit only if ALL three hold:** (a) a real third-party v1 plugin ecosystem
exists, (b) multiple users articulate a gating need not expressible as core
config, and (c) the fail-closed security review is funded. Until then, answer
gating pressure with **declarative core policy config** (path patterns, spend
thresholds, branch rules) — ~90% of the value, none of the trust problem — and
note that v1 plugins already get surprisingly far by **calling back into warden
as a normal client** (`wd send-message`, `wd snapshot`, …) on observed events.

**What IS worth building instead** (grows the ecosystem that could ever justify
v2): 2–3 more official observer plugins (chat webhook, metrics JSONL, the
OS-notifier as a released artifact) and, once ≥3 plugins exist, a **plugin
manager** (`warden plugin install`) with a signed index + pinned SHA256 —
install ≠ enable, `plugins.enabled` stays off by default. Spec-first when
picked up.

---

## 📝 Notes

- **Design specs** live in `docs/specs/` (current) and `docs/superpowers/specs/`
  (older) — check both before starting.
- **Shipped features** are catalogued in [FEATURES.md](FEATURES.md); usage in
  [USAGE.md](USAGE.md).
- Effort estimates are approximate.
- The open follow-ups are independent of one another. Among the parked items,
  **distributed warden (#14)** depends on the hub.

---

## 🤝 Contributing

When implementing features from this roadmap:

1. Check for a design spec in `docs/specs/` or `docs/superpowers/specs/`
2. Write tests first (TDD where possible)
3. Update docs (README, FEATURES.md, USAGE.md, the website, the skill)
4. Run `make verify` before committing
5. **Update this file:** document the finished feature in FEATURES.md and delete its
   entry here, so the roadmap stays a pure to-do list

---

**Questions or suggestions?** Open an issue at https://github.com/srjn45/warden/issues
