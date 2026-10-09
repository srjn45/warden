# Warden Future Enhancements & Feature Roadmap

**Last Updated:** 2026-10-09
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

#### 57. CLI product-surface and safety review — *deferred until the PTY permission runtime lands*
**Effort:** 1–2 weeks, best split into coordination/approval and operator/admin
tracks

The principal agent, pipeline, autopilot, schedule, plan, project/workspace,
and git/check command families have already had focused consistency work. The
remaining command surface needs a product review after the local
`pty-runtime-and-permission-decision-engine` is proven. Do not undertake this
as a cosmetic rename sweep: retain commands only where they solve a concrete
operator or automation problem, preserve scripts through hidden compatibility
aliases, and make safe/read-only behavior the default.

**First priority — approval, context, and messaging:**

- Replace the current auto-approve-centric UX with the PTY runtime's explicit
  permission model: recognized permission/trust prompts, effective capability
  posture, scope, available choices, confidence/evidence, decision/audit
  history, and strategic-question escalation must be visible separately.
- Target a durable surface such as `approval list`, `approval answer`,
  `approval posture show|set`, `approval policy list|add|remove`,
  `approval diagnostics known-prompts …`, and `approval audit`. Keep the old
  `approval auto …` rule/toggle commands as compatibility only during a staged
  migration; they cannot be the long-term representation of read/write,
  allowlisted execution, or `dangerously-execute-all` policy.
- Make `message list` read-only and add explicit `message ack`/`consume`;
  today merely inspecting `message inbox` marks messages read. Keep `wait` for
  scripts/agents, with stable timeout and JSON behavior.
- Keep `context` as an agent/pipeline coordination primitive (`set`, `cas`,
  `append`, `get`, `list`, `delete`), but document scope/retention/size limits,
  add structured output where appropriate, and record authenticated writer
  provenance separately from any user-supplied `--as` identity.

**Backend, usage, and inspection clarity:**

- Retain AI-CLI registry, discovery, model catalog, quota, spend, savings,
  resource, search, history, audit, and recovery capabilities; they solve real
  fleet-operation problems. Use "AI CLI" in user-facing text where "backend"
  is an implementation term.
- Hide the retired no-op `backend suggest` as compatibility-only. Relocate the
  interactive fleet REPL from `backend repl` to a neutral `repl`/`console`
  command, retaining the old path as an alias.
- Disambiguate the live provider model menu from Warden's routing catalog
  (`backend model available` versus `backend model list`) and distinguish
  backend billing tiers from model routing tiers in names/help.
- Keep provider quota as `usage`, but move non-financial historical analysis
  from `usage insights` to `inspect insights` canonically. Make `usage spend`
  accurately describe provider coverage and keep estimated dollar values
  visibly distinct from exact token counts or invoices.
- Retain operator-triggered quota recovery, but make its dry-run/apply boundary
  unmistakable and consider a more operational home such as backend quota
  reconciliation.
- Reframe `inspect export|import` as agent-record metadata migration, not
  backup/recovery: they do not restore worktrees, branches, or terminals. Add
  dry-run and explicit confirmation before overwrite/merge.
- Make every `inspect repair …` command diagnose by default and require an
  explicit `--apply` for mutation; the current safe-looking `--dry-run` opt-in
  is too easy to misuse.

**Configuration, daemon, and operator safety:**

- Keep `config`, `daemon`, MCP serving, token lifecycle, completion, doctor,
  bug reporting, setup, tutorial, TUI, update, and version. These are real
  installation and operational needs, not gratuitous commands.
- Keep `doctor` read-only. Move mutating membership reconciliation out of
  `doctor --reconcile-membership` to a dedicated `inspect repair` operation.
- Make `config init` migration previewable, backup existing configuration, and
  require an explicit apply step when it would rewrite an existing file.
- Harden bearer-token display/rotation: secret output should require an
  explicit reveal/machine-use mode and protect ordinary interactive users from
  accidental terminal/log disclosure. Keep separate ephemeral generation and
  persisted-service rotation because they solve different deployment cases.
- Require an explicit reset scope for `factory-reset`; retain the drain,
  backup, and confirmation model, but never make a broad data wipe an implicit
  default choice.
- Move the TUI's web-cockpit rebuild escape hatch to a daemon/cockpit repair
  command; it is not normal TUI presentation behavior. Keep `--tmux-native` as
  an advanced presentation option while tmux remains supported.
- Make setup backend-neutral: validate Warden's core prerequisites and the
  user's selected AI CLI rather than treating Claude as universally required.
  Update tutorial language to current canonical commands and Cockpit terms.
- Describe `daemon mcp` as usable by any compatible MCP client, not as
  Claude-specific. Add machine-readable output to update/check paths where
  automation needs it.
- Keep Hub device login hidden/experimental or place it under a Hub namespace
  until remote Hub enrollment is a generally deployable product capability.

**Migration guardrails:**

- Do not remove legacy command paths abruptly. Canonical replacements first,
  then hidden aliases, stderr-only notices that are suppressed for JSON and
  machine modes, telemetry only if separately approved, and a later removal
  decision backed by use evidence.
- Preserve automation contracts: stable JSON, raw stdout for intentional shell
  composition, explicit exit behavior for waits/timeouts, and no destructive
  action caused merely by a status or inspection command.

**Revisit only after:** `pty-runtime-and-permission-decision-engine` is merged
and operating as the authoritative permission/prompt decision path. The
approval redesign must be derived from its actual event, capability, audit, and
strategic-routing contracts rather than bolting more rules onto the current
poller-era auto-approve interface.

---

## 🧊 Parked (need a concrete demand signal)

#### 56. Hub-backed remote PTY terminal streaming — *deferred until Hub work resumes*
**Effort:** 2–4 weeks after the local Warden-owned PTY runtime is proven

When Warden Hub is developed, extend the local Warden node with secure remote
access to managed PTY sessions for web and mobile clients. This is explicitly
**not** part of the local PTY runtime rollout.

- The local daemon remains the sole owner of the AI CLI/PTY process and the
  only component allowed to write terminal bytes. Hub is an authenticated relay
  and fan-out control plane, never a direct shell, tmux, filesystem, or PTY
  endpoint.
- Use one authenticated, multiplexed node-to-Hub connection (WebSocket is an
  acceptable initial transport). Logical channels will carry terminal render
  output, normalized events, input, resize, controller leases, approvals,
  lifecycle/status, and metrics. Consider WebTransport/QUIC only after the
  initial design is working.
- Separate observer access from interactive-controller access: default to one
  controller and many viewers; require explicit request, takeover, revocation,
  expiry, disconnect, local-user-priority, and audit semantics.
- Remote input follows `web/mobile → Hub → local daemon → PTY`. The daemon must
  recheck authorization, controller lease, input ownership, session state,
  limits, prompt freshness, and the effective local permission posture before
  every write. Permission approval remains distinct from raw terminal control.
- Maintain two data paths: a low-latency, bounded, coalescible render stream
  for interactive terminal display, and a durable, normalized, redacted,
  ordered, cursor-addressable event history for replay, audit, decisions, and
  debugging. Slow clients or Hub outages must never block the AI CLI or cause
  unbounded resource use.
- Web should support observe/control, reconnect/resume, resize, controller
  state, and accessible approval/status views. Mobile should begin view-first,
  with deliberately gated control rather than unrestricted desktop-terminal
  emulation. Detach must never stop the local agent.
- Design and test protocol versioning, reconnect/resume, frame loss/reorder,
  stale input, controller races, authorization revocation, secret redaction,
  audit/replay integrity, backpressure, resource pressure, and node/Hub/client
  restart recovery.

**Revisit only after:** `pty-runtime-and-permission-decision-engine` is merged,
deployed, the authoritative local PTY attach/recovery path is proven, and Hub
work is actively being planned. Build on the existing node/Hub remote-access
identity and authorization boundaries rather than bypassing them.

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
