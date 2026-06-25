# warden interactive tutorial — design

**Date:** 2026-06-25
**Status:** Shipped
**Feature:** `wd tutorial` — a first-run guided walkthrough of warden's core loop, plus a non-blocking first-run hint that nudges a new operator toward it. Roadmap item #42 (Documentation & Onboarding).

## Motivation

A new operator who installs warden lands at a bare prompt with ~40 verbs and no
obvious starting point. The roadmap (#42) called for a "first-run guided
walkthrough (detect `~/.warden/tutorial-complete`)" — a cheap way to teach the
**core loop** (spawn → watch → talk → tear down) and point at the two richer
surfaces (the cockpit TUI and the web GUI) without the operator hunting through
`--help`.

The bar is deliberately low: this is onboarding polish, not a framework. It must
teach the loop in one screen, **change nothing**, and — critically — **never get
in the way** of the experienced user or of automation. A nag that fires in a
pipeline or a daemon log is worse than no tutorial at all.

## Goals

- `wd tutorial` — an explicit, idempotent walkthrough the operator can run anytime.
- A `tutorial-complete` marker in `<data_dir>` so it doesn't nag once seen.
- `--skip` (mark done without running) and `--reset` (clear the marker, run fresh).
- A **non-blocking** first-run hint pointing at the command, surfaced only when it
  is safe and useful to do so.
- Pure, unit-tested helpers; no heavy TUI dependency; no daemon change.

## Non-goals

- **Auto-running** the walkthrough, or blocking/hijacking any command — the hint is
  one stderr line, nothing more.
- An interactive, keypress-stepped UI — the walkthrough prints all steps in one
  pass (simple, pipe-safe, testable). A TUI tour is out of scope.
- Teaching every verb — the tour covers the core loop and the two surfaces; deeper
  docs live in FEATURES.md.

## The marker-gated first-run model

The completion marker is a single file, `<data_dir>/tutorial-complete`, sitting
next to the rest of warden's state (sessions, inbox, snapshots). Its **presence**
is the whole gate: present ⇒ the operator has seen (or skipped) the tour ⇒ stay
silent. The marker body is a human-readable timestamped line; only its existence
matters.

`data_dir` is resolved from config (`config.Load(configPathFor(cmd)).DataDir`),
exactly as the audit/daemon paths are — so `--config`/`data_dir` overrides are
honoured and tests can point it at a temp dir.

Three entry points write or clear it:

- **`wd tutorial`** prints every step, then writes the marker.
- **`wd tutorial --skip`** writes the marker **without** printing steps.
- **`wd tutorial --reset`** removes the marker (idempotent — a missing marker is
  success). `--reset` and `--skip` are mutually exclusive.

`writeTutorialMarker` `MkdirAll`s the data dir first, since the tutorial may run
before the daemon has created it.

## The first-run hint

A `PersistentPreRun` on the root command calls `maybeHintTutorial` before any
subcommand runs. It is **best-effort and never errors**, and emits at most one
line to **stderr** (so it can never corrupt a command's stdout, e.g. `--json`).

The decision is a pure predicate, `shouldHintTutorial(markerExists, isTTY,
gateOn, cmdName)`, which returns true **only** when **all** hold:

- the marker is **absent**, and
- output is a real **TTY** (keyed on stdout via the existing `isTTY` helper), and
- the `tutorial` config gate is **on** (default on), and
- the invoked command is **not** suppressed.

Suppressed commands (`tutorialHintSuppressedCmds`) are the machine/automation and
full-screen/self-referential surfaces: `daemon`, `mcp`, `hook`, `guard`,
`completion`, `orch`, the cockpit root (`warden`), `tui`, and `tutorial` itself.

## TTY / non-interactive safety invariants

- The hint is **stderr-only** and **one line** — stdout (including JSON output) is
  never touched.
- **Non-TTY ⇒ silent.** Piped output, captured output, and every CLI test (which
  drives the command with a `bytes.Buffer`) get no hint. This is what keeps the
  existing `runCLI`-based tests hint-free without special-casing.
- **Machine surfaces ⇒ silent.** The daemon, the MCP stdio server, hooks, and
  shell-completion output never carry the hint, regardless of TTY.
- **Gate ⇒ silent.** `tutorial: false` disables the hint entirely.
- The walkthrough is **never auto-run**; the hint only *mentions* `wd tutorial`.

## Reuse, not rebuild

- Cobra command shape, flags, and `cmd.OutOrStdout()` plumbing mirror the thin
  verbs (`rotate.go`, `snapshot.go`).
- `isTTY` (sessions.go) is reused for the TTY check — no new dependency.
- `config.Load`/`configPathFor` + a new `tutorial` gate follow the existing
  gate pattern (struct field + `schema` hint + `defaults()` + `Get*` accessor;
  the drift-guard test keeps schema and struct in sync).
- No new client method or daemon endpoint — this is a local-only verb.

## Testing

- **Marker round-trip** against a temp dir: write ⇒ done + readable body; reset ⇒
  not done; reset is idempotent; `writeTutorialMarker` creates a missing data dir.
- **Suppression predicate** `shouldHintTutorial`: the single happy path (fresh +
  TTY + gate-on + normal command ⇒ hint) and each suppression condition
  independently (marker present / non-TTY / gate-off / each suppressed command ⇒
  no hint).
- **Command behaviour** end-to-end via the root command with an isolated
  `--config`: `--skip` writes the marker but prints no steps; a full run writes
  the marker *and* prints the steps; `--reset` removes the marker;
  `--reset --skip` errors.
- **Auto-prompt safety**: a non-suppressed command run with a buffer (non-TTY)
  emits no hint — the contract that keeps the rest of the CLI suite quiet.
- No real TTY or interactive blocking in any test: the engine is a pure
  `renderTutorial(io.Writer, steps)` and the steps are plain data.

## References

- FEATURES.md §24 (First-run tutorial)
- `internal/cli/{tutorial,tutorial_test}.go`
- `internal/cli/root.go` (the `PersistentPreRun` hint wiring)
- `internal/config/config.go` (the `tutorial` gate)
- `internal/cli/rotate.go` / `snapshot.go` (the thin-verb + pure-helper pattern this mirrors)
