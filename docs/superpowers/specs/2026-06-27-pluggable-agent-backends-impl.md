# Pluggable Agent Backends — Implementation Plan

**Status:** Implementation plan (companion to the design spec)
**Date:** 2026-06-27
**Design:** `2026-06-27-pluggable-agent-backends-design.md`
**Branch (design):** `design/pluggable-agent-backends` (PR #153)

> Companion to the design spec. The design says *what* and *why*; this says
> *which files, in what order, behind which tests*. Each phase is an independent,
> shippable PR. **Phase 0 must merge before any backend adapter.**

---

## Guiding constraints

- **Phase 0 is zero-behavior-change.** Pure refactor. Every existing test passes
  unchanged; no new user-visible behavior. This de-risks the whole effort.
- **TDD per repo norm** (CLAUDE.md): write/port tests first.
- **Spec-first for any daemon API** surface (`openapi.yaml` → `make generate`;
  never hand-write handlers). Backend selection likely needs a new spawn field.
- **Definition of Done** (CLAUDE.md): README + FEATURES (both) + USAGE + website +
  CLI help + skill, then tag/release (confirm before pushing `v*`).

---

## Phase 0 — Extract the adapter interface (zero behavior change)

**Goal:** introduce `internal/agentbackend` and route lifecycle through it, with
Claude Code as the only registered backend. Nothing else changes.

### 0.1 New package `internal/agentbackend`
- `backend.go` — the `Backend` interface, `Caps`, neutral `Turn`/`State`/
  `Approval`/`PricingTable`/`LaunchOpts`/`ResumeOpts` types (design §4).
- `registry.go` — `Register`/`Get`/`Default()` (default `"claude"`).
- `backends/claude.go` — the Claude adapter. **Move, don't rewrite:**
  - `claudeBase`/`claudeLaunch`/`claudeResume`/`permissionFlag` → `LaunchCmd`/`ResumeCmd`.
  - `classifyArg`/`summaryArg` + `runClaudeP` → `HeadlessCmd`.
  - `claudeProjectDir` + `transcriptPath` glob logic → `TranscriptPath`.
  - `digest/parse.go` JSONL parsing → wrapped by Claude's `ParseTranscript`
    (keep `digest.ParseTranscript` callable; adapter delegates to it initially).
  - `approval` package detectors → Claude's `DetectState`/`ParseApproval`
    (adapter delegates to existing `approval` funcs at first; physical move later).
  - `Caps`: everything true; `PermissionModes = PermissionModes` (existing).

### 0.2 Wire `Lifecycle` to the registry
- Add `Backend agentbackend.Backend` resolution inside `Lifecycle` (default via
  `agentbackend.Default()`); keep `New(r, cfg)` signature, resolve per-session.
- Replace literal `claudeLaunch(...)` / `claudeResume(...)` call sites in
  `Spawn` (`:886`) and `SpawnJob` (`:1572`) with `backend.LaunchCmd(...)` /
  `ResumeCmd(...)`. The hint fragments (`pipelineHint`/`collabHint`/
  `gitConventionsHint`) stay appended by lifecycle for now — they become
  `SystemPromptFlag` in Phase 1 once a second backend needs them differently.
- `runClaudeP` (classify/summarize) → `backend.HeadlessCmd`; keep the existing
  local-LLM fallback ahead of it (unchanged).
- `transcriptPath(sess)` → `backend.TranscriptPath(l.ProjectsDir, sess.Workdir,
  sess.ClaudeSessionID)`.

### 0.3 Session model
- `internal/store/types.go:142` — add `Backend string json:"backend,omitempty"`
  to `Session` (empty ⇒ `"claude"`, back-compat for existing stores).
- **No migration needed** — empty field reads as Claude default. Note in
  `ClaudeSessionID`: rename semantics to "backend session id" in comments only
  (keep the JSON key for back-compat to avoid a store migration this phase).

### 0.4 Tests
- Port `lifecycle_test.go`'s `claudeLaunch`/`claudeResume`/`claudeProjectDir`
  helpers to call through the Claude adapter; assertions stay identical.
- New `agentbackend/registry_test.go`: register/get/default.
- `go vet ./... && make verify` green; **zero golden-output diffs**.

**Exit criteria:** all existing tests pass unmodified in behavior; the string
typed into tmux for a Claude spawn is byte-identical to today.

---

## Phase 1 — Aider adapter (the proof) + selection plumbing

**Goal:** prove the interface end-to-end with the easiest real backend, and add
the user-facing backend-selection path + capability-gated degradation.

### 1.1 Backend selection plumbing
- **Daemon API (spec-first):** add `backend` to the spawn request in
  `openapi.yaml`; `make generate`. (See memory: spec-first, never hand-write.)
- **CLI:** `wd spawn --backend <id>` flag in `internal/cli/` (cobra). Default
  `claude`. Help text + `reference/cli.md`.
- **MCP:** add `backend` param to `spawn_agent` (`internal/mcp/`), kept at parity
  with CLI (memory: features-catalog MCP/CLI parity).
- Persist to `Session.Backend`; resolve adapter in lifecycle from it.

### 1.2 Capability-gated degradation (the framework)
Implement the §5 rules so a non-full backend never crashes:
- `!StructuredTranscript` ⇒ digest narrator falls back to pane-scrape summary;
  savings recording disabled for that agent (guard `SavingsHook` calls).
- `!Headless` ⇒ classify/summarize always take the local-LLM path.
- `!Pricing` ⇒ `wd spend` shows tokens (heuristic) not dollars; `wd savings`
  omits the agent.
- `!Resume` ⇒ rotate/handoff re-spawn fresh instead of `--resume`.
- `!SessionIDControl` ⇒ **discover-then-pin**: capture the agent-generated
  session id from first output/store file instead of assigning it.

### 1.3 `backends/aider.go`
- `LaunchCmd`: `aider --model <m> [--yes-always]` (interactive in tmux).
- `HeadlessCmd`: `aider --message <prompt>` (or `--message-file`).
- `ResumeCmd`: `ok=false` (repo-history resume; `Caps.Resume=false`).
- `TranscriptPath`: `<workdir>/.aider.chat.history.md`; `ParseTranscript`: a
  **markdown** parser → `[]Turn` (forcing function for the non-JSONL path). If
  fidelity is poor, set `Caps.StructuredTranscript=false` and ship Aider as
  Tier B (degraded digests) — decide from real output.
- `DetectState`/`ParseApproval`: simple y/n regex (`(y)es/(n)o`), far simpler
  than Claude's box-drawing parser.
- `Caps`: `SessionIDControl=false`, `Resume=false`, `Headless=true`,
  `PermissionModes=["default","yes-always"]`, `Pricing=false` (BYO model).

### 1.4 Testing (free — see design §13)
- `uv tool install aider` + **Ollama** (both $0) on the dev box.
- Capture a real `.aider.chat.history.md` and a real approval prompt; commit
  fixtures under `internal/agentbackend/backends/testdata/aider/`.
- Unit-test `ParseTranscript`/`DetectState` against fixtures.

**Exit criteria:** `wd spawn --backend aider` launches, attaches, runs git
lifecycle, and either produces a digest (Tier A) or cleanly degrades (Tier B),
with Claude behavior unchanged.

---

## Phase 2 — Antigravity CLI (`agy`) adapter (headline)

**Goal:** first first-class non-Claude backend.

### 2.1 Resolve open questions FIRST (hands-on, design §7 Q1)
- Install `agy` (free Gmail tier, design §13). Run a session.
- **Find the transcript store + format** → decides Tier A vs B. Inspect
  `~/.antigravity` / `~/.config/antigravity` (Gemini used
  `~/.gemini/tmp/<hash>/chats/`).
- Capture an approval prompt + the status-bar idle/working signal.

### 2.2 `backends/antigravity.go`
- `LaunchCmd`: `agy [--model <m>]` (TUI in tmux); `ResumeCmd`: `agy --conversation
  <id>` (`Caps.Resume=true`); `HeadlessCmd`: `agy -p <prompt> --output-format json`.
- Permission: `--headless --approve <policy>`.
- `TranscriptPath`/`ParseTranscript`: per 2.1 findings.
- `DetectState`: parse the status bar (running-subagents indicator) — **not** the
  Claude "esc to interrupt" signal.

### 2.3 Subagent attribution caveat
- Document that `wd spend` granularity drops for Antigravity (internal subagents
  roll into one session). Do **not** fabricate per-subagent numbers.
- Document the orchestration boundary: warden drives the top-level `agy` session
  as one warden agent; it does not manage `agy`'s internal subagents.

**Exit criteria:** `wd spawn --backend antigravity` is first-class; transcript
tier decided and documented.

---

## Phase 3+ — Codex CLI, OpenCode, then catalog (design §12)

Each its own PR reusing the proven interface:
- **Codex CLI** — sandbox exec, JSON output, resume.
- **OpenCode** — **SQLite** session store ⇒ `TranscriptSource` variant (DB query,
  not file read); validates the non-file transcript path. Most-starred OSS.
- Then promote from the §12.1 catalog on real demand signals — not speculatively.

---

## Cross-cutting work (lands with Phase 1)

- **Docs (DoD):** README supported-agents list; `FEATURES.md` (root matrix +
  `docs/FEATURES.md` prose) + website mirror; `docs/USAGE.md` backend selection;
  website guide + `reference/cli.md`; `skills/warden/` note that backends differ.
- **CLI help:** `--backend` flag `Short`/`Long` synced to `reference/cli.md`.
- **Release:** one tag per backend (minor for abstraction + first backend; patch
  per later backend). Confirm before pushing `v*` (memory: tagging style;
  batch ≤3 tags per push).

---

## Risk register

| Risk | Mitigation |
|---|---|
| Phase 0 accidentally changes Claude behavior | Byte-identical tmux-string assertion; port (not rewrite) existing tests |
| Antigravity transcript not parseable | Tier-B degradation path already built in Phase 1; document, don't block |
| Subagent cost attribution confusion | Document limitation; tokens-not-dollars for that backend |
| Approval-UI divergence across agents | Per-adapter detectors now; declarative marker DSL only after ≥3 share a shape |
| Store migration risk from new field | `backend` is `omitempty`, empty ⇒ claude; no migration |
| Backends couple into many subsystems | Capability flags + degradation keep core neutral; adapters are isolated PRs |
