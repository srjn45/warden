# Warden Orchestrator — Phase C: Embedded Shell + `!` Passthrough + Cockpit Pane Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the orchestrator *run on top of the operator's own shell*. A `!`-prefixed line is a raw command submitted to a persistent child `$SHELL`; the orchestrator streams its output verbatim, observes it as conversation context, and **on error does nothing until asked**. Then point the cockpit's master pane at `warden orch` so this becomes the bottom-left pane's face, with a one-keypress escape hatch back to a bare shell.

**Design spec:** `docs/superpowers/specs/2026-06-25-warden-orchestrator-design.md` (Phase C + "It hosts the operator's shell — and stays passive over it")

**Depends on:** Phase B (the `wd orch` REPL, `Session`, `RunREPL`). Independent of Phase D — build in either order.

**Architecture:** Two pieces. (1) An embedded-shell host inside `internal/orchestrator`: a persistent PTY-backed `$SHELL` session that `RunREPL` routes `!`-lines to, teeing output to the operator's screen (verbatim, live) and into a tail-bounded capture buffer the model reads. (2) A one-line `buildCockpit` change so the master pane runs `self + " orch"` instead of bare `$SHELL`, preserving spawn-dir semantics because the embedded shell *is* a real persistent shell started in `masterCwd`.

```
  RunREPL (Phase B)
    line starts with "!" ─▶ Shell.Run(cmd) ─┬─▶ os.Stdout   (verbatim, live)
                                            └─▶ capture buf  (tail-truncated → model context)
    bare line            ─▶ Session.Handle  (unchanged)
  internal/orchestrator/shell.go   — persistent PTY $SHELL host
  internal/tui/compositor.go:83    — master pane cmd: $SHELL → self+" orch"
```

**Tech Stack:** Go 1.26+. PTY via `github.com/creack/pty` (the standard Go PTY lib) — **the one new dependency**; if the repo already vendors a PTY helper, reuse it (check `go.mod` first and prefer the existing one). Reuses `truncateTail`/`maxCheckOutputLines` discipline from `internal/lifecycle/check.go` for the capture cap.

**Scope guard:** **`!` is non-interactive only.** Run the command, stream to completion, return to the prompt. Full-screen/interactive programs (pager, `vim`, a REPL) are **out of scope** — those belong in the raw-`$SHELL` escape hatch. The orchestrator **takes no action on `!` output** (no auto-diagnose/fix/spawn) — it reports verbatim and waits. If a task here starts parsing or reacting to `!` output, stop: that violates the passive-observation invariant.

---

## File Structure

### New Files
- `internal/orchestrator/shell.go` — the persistent PTY `$SHELL` host (`Shell`, `Run`, capture buffer)
- `internal/orchestrator/shell_test.go` — passthrough, verbatim capture, exit-code surfacing, capture cap

### Modified Files
- `internal/orchestrator/session.go` — `RunREPL` recognizes a `!` prefix and routes to the `Shell`; `Session` gains the captured `!` output as observed (not acted-on) context
- `internal/tui/compositor.go` — master pane command (line ~83) becomes `self + " orch"` (config-gated by `orchestrator`); add the escape-hatch keybinding
- `go.mod` / `go.sum` — `creack/pty` if not already present

---

## Task 1: The embedded `$SHELL` host

**Files:** New `internal/orchestrator/shell.go`, `internal/orchestrator/shell_test.go`

The whole point is **the operator's own shell**, not a reimplemented mini-shell. So it runs the real `$SHELL` as a login/interactive shell that sources the operator's rc/profile — aliases, functions, `PATH`, env, and `--help`/usage output are identical to their normal terminal. warden adds capability above the shell and changes nothing about it.

- [ ] **Step 1: Tests first**

PTY behavior is hard to unit-test hermetically, so test against `/bin/sh -c` semantics with deterministic commands, and gate the PTY-specific assertions behind a skip when no PTY is available (CI containers). Cover:

  - **Passthrough runs in a real shell:** `Run("echo $((1+1))")` returns output containing `2` and exit code 0.
  - **State persists across calls:** `Run("cd /tmp")` then `Run("pwd")` → `/tmp` (proves a *persistent* session, the property that preserves spawn-dir/`cd` semantics).
  - **Output is captured verbatim:** the capture buffer equals what was written to the screen writer — no paraphrase, no trimming beyond the cap.
  - **Non-zero exit is surfaced, not swallowed:** `Run("exit 3")`-style command reports exit code 3; the host returns it but **takes no further action**.
  - **Capture is tail-bounded:** a command emitting > `maxCaptureLines` lines yields a capture buffer truncated to the tail (operator's screen still gets the full stream — assert the screen writer got all lines, the capture got the tail).

```go
func TestShell_PersistsCwd(t *testing.T) {
    sh, err := NewShell("/tmp", io.Discard)
    require.NoError(t, err)
    defer sh.Close()
    _, _ = sh.Run(context.Background(), "cd /tmp")
    out, _ := sh.Run(context.Background(), "pwd")
    require.Contains(t, out.Captured, "/tmp")
}

func TestShell_CaptureIsTailBounded(t *testing.T) {
    var screen bytes.Buffer
    sh, _ := NewShell("", &screen)
    defer sh.Close()
    out, _ := sh.Run(context.Background(), "seq 1 1000")
    require.LessOrEqual(t, strings.Count(out.Captured, "\n"), maxCaptureLines)
    require.Contains(t, screen.String(), "1000", "the operator always sees the full stream")
}
```

- [ ] **Step 2: Implement `shell.go`**

```go
const maxCaptureLines = 200 // model-context cap; the operator's screen is never truncated

type RunResult struct {
    Captured string // tail-bounded, for the model
    ExitCode int
}

// Shell hosts a persistent child $SHELL on a PTY. It is the operator's own shell
// (login/interactive, sources rc/profile) so aliases/functions/PATH/help are
// identical to their terminal. warden adds capability above it; it changes
// nothing about the shell.
type Shell struct {
    cmd    *exec.Cmd
    ptmx   *os.File
    screen io.Writer
    mu     sync.Mutex
}

// NewShell starts $SHELL (defaulting to /bin/sh) in dir, on a PTY, teeing all
// output to screen. dir seeds the shell's cwd, preserving the spawn-dir
// semantics the cockpit's master pane relies on.
func NewShell(dir string, screen io.Writer) (*Shell, error) { /* pty.Start the login shell */ }

// Run submits one NON-INTERACTIVE command, streams its output verbatim to the
// screen and a tail-bounded capture buffer, and returns when it completes.
// Interactive/full-screen programs are out of scope (run those in the raw-shell
// escape hatch). Run never acts on the output — it reports and returns.
func (s *Shell) Run(ctx context.Context, line string) (RunResult, error) { /* write line+\n, read until prompt sentinel, tee */ }

func (s *Shell) Close() error { /* signal + close ptmx */ }
```

Implementation notes:
  - **Command completion** for a persistent PTY shell needs a boundary marker: write the command followed by a unique sentinel echo (e.g. `printf '\n<warden-eot-NNN>%s\n' "$?"`), read until the sentinel, and parse the trailing `$?` for the exit code. This keeps "run to completion, return to prompt" deterministic without a fragile prompt-regex.
  - **Verbatim tee:** copy the PTY read stream to `screen` and to a ring/line-capped buffer simultaneously; never transform bytes. Strip only the sentinel line from the *captured* text (the operator's screen shows the raw stream minus our injected sentinel — keep that injection invisible by emitting it on a line we filter from the screen writer too).
  - **Capture cap:** reuse `truncateTail`-style logic (lifecycle/check.go:212) so a chatty command can't blow the model's context.

- [ ] **Step 3: Run → fail → implement → pass → commit**

Commit: `feat(orchestrator): persistent PTY $SHELL host with verbatim tee + tail-bounded capture`.

---

## Task 2: Wire `!` into the REPL — observe, report, stay passive

**Files:** Modified `internal/orchestrator/session.go` (+ targeted tests in `session_test.go`)

- [ ] **Step 1: Tests first**

  - **`!`-line routes to the shell, bare line to the model:** a REPL fed `!echo hi\n` calls `Shell.Run`, not `Session.Handle`; a bare line does the reverse.
  - **Output is reported verbatim:** the REPL prints exactly what the shell streamed; no orchestrator commentary is added around a successful `!` command.
  - **On error, the orchestrator does nothing:** feed `!false\n` (non-zero exit) → the REPL surfaces the output/exit code and returns to the prompt with **no** model turn, no spawn, no diagnosis. Assert `Session.Handle` / `Chatter.Chat` were not called.
  - **The command + output enter context but are not acted on:** the next *bare* line's `Chat` call includes the prior `!` command and its (tail-bounded) output as observed context — so "what went wrong?" works — but nothing happened until that bare line.

```go
func TestREPL_BangErrorTakesNoAction(t *testing.T) {
    chat := &recordingChatter{}
    h := newTestHost(chat, fakeShell{result: RunResult{Captured: "boom", ExitCode: 1}})
    h.feed("!make build\n") // a failing command
    require.Zero(t, chat.calls, "on error the orchestrator does nothing until the operator asks")
}
```

- [ ] **Step 2: Implement**

In `RunREPL`, branch on a leading `!` (after trimming leading space): strip it, `Shell.Run` the remainder, print the result, and append a `RoleUser`/`RoleTool`-style observation to `Session`'s history *as context only* — never trigger a `Chat` turn from a `!` line. A bare line proceeds to `Session.Handle` exactly as in Phase B. `Session` gets a small method to record an observed shell result into its message history without invoking the model.

Keep the passivity invariant front and center in a code comment: **`!` output is observed and reported; the orchestrator initiates no action from it.**

- [ ] **Step 3: Run → pass → commit**

Commit: `feat(orchestrator): ! passthrough — run in the operator's shell, report verbatim, stay passive on error`.

---

## Task 3: Cockpit master pane hosts the orchestrator

**Files:** Modified `internal/tui/compositor.go`, `internal/tui/compositor_test.go` (or the existing cockpit test)

The master pane (`compositor.go:83`, the `masterID` split running `$SHELL`) becomes the orchestrator's home. The change is deliberately minimal — only the pane's *command* — because the embedded shell preserves everything the pane did before (cwd seeds `n`-spawned agents; `cd`/env persist).

- [ ] **Step 1: Tests first**

  - With `orchestrator: true`, `buildCockpit` issues the master split with `self + " orch"` as the pane command.
  - With `orchestrator: false` (default), it stays bare `$SHELL` (no behavior change for anyone who hasn't opted in).
  - The pane still runs in `masterCwd` (spawn-dir semantics preserved) — assert the `-c masterCwd` flag is unchanged on the master split.
  - Layout, pane ids, `remain-on-exit`, mouse, and Alt-arrow bindings are byte-for-byte unchanged (regression guard on the rest of `buildCockpit`).

- [ ] **Step 2: Implement**

Thread the `orchestrator` config bool into `cockpitOpts` and choose the master pane command:

```go
masterCmd := shell // existing $SHELL default
if o.orchestrator {
    masterCmd = o.self + " orch"
}
masterID, err := runPaneCreate(ctx, run,
    "split-window", "-h", "-b", "-l", "40%", "-t", detailID, "-c", o.masterCwd,
    "-P", "-F", "#{pane_id}", masterCmd)
```

- [ ] **Step 3: Escape hatch to a raw shell**

Reuse the already-planned [2026-06-09-tui-master-pane-shell-toggle](2026-06-09-tui-master-pane-shell-toggle.md) mechanism: a keybinding that `respawn-pane`s the master pane to bare `$SHELL` (and back). The `orchestrator` config flag only sets which face the pane *starts* on; the toggle works regardless. If that toggle plan hasn't landed, ship this task with the config-gated start-face and a follow-up note — do not block the pane-command change on the toggle.

- [ ] **Step 4: Full suite + commit**

```bash
cd /home/srjn45/dev/warden && go test ./internal/tui/... ./internal/orchestrator/... && go build ./...
```

Commit: `feat(tui): cockpit master pane hosts the orchestrator-over-shell (config-gated)`.

---

## Summary

Three TDD tasks. Two new behaviors (`shell.go` host + `!` routing) and one minimal cockpit wiring change.

1. ✅ Persistent PTY `$SHELL` host — the operator's *own* shell, verbatim tee to screen, tail-bounded capture for the model.
2. ✅ `!` passthrough — bare lines to the model, `!`-lines to the shell, output reported verbatim, **no action on error** until asked.
3. ✅ Cockpit master pane runs `wd orch` when `orchestrator: true`, with a raw-`$SHELL` escape hatch one keypress away; spawn-dir semantics preserved.

**MVP boundary held:** `!` is non-interactive only; interactive/full-screen programs are deferred (run them in the escape-hatch shell). The orchestrator observes and reports — it never interprets, rewrites, or acts on `!` output unprompted.

**Deferred follow-up:** interactive `!` (wire the PTY through to the operator's terminal for a command's lifetime, hand control back on exit) — a clean, separable enhancement once the non-interactive path is proven.
