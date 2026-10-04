# Fast-Brain: Crash Triage, User-Approved Bug Reporting & TUI Log Viewer

**Date:** 2026-10-04  
**Status:** Approved design spec  
**Scope:** Fast-Brain automated crash triage, privacy-safe user-approved GitHub issue generation for pre-compiled binary users, and full-screen TUI log viewer (`l` hotkey) inside BubbleTea.

---

## 1. Problem & Context

### A. Binary Distribution Reality
Warden is distributed as pre-compiled static binaries (`install.sh` / `warden update`). When an internal bug or panic occurs:
1. End-users do not have the Go source code or compiler to fix it.
2. Manually assembling stack traces and reporting issues on GitHub is high-friction.
3. Users fear leaking API keys, private tokens, or proprietary paths in public issues.
4. Many crashes are operational (e.g. 429 rate limits or test failures in the user's project), not Warden bugs.

### B. TUI Display Disruption
In the BubbleTea TUI cockpit, raw log lines written to stderr/stdout by background processes or external tools corrupt the terminal alternate screen buffer, causing visual glitches. Operators also lack an in-product way to inspect recent logs without switching to another terminal window.

---

## 2. Fast-Brain Crash Triage (`DiagnoseFailure`)

When an agent process terminates with non-zero exit or an unhandled panic occurs:
1. The daemon captures the exit code, signal, active command, and the last 40 lines of stderr/pane excerpt.
2. Fast-Brain evaluates the excerpt via `fastbrain.Engine.Decide(KindDiagnoseFailure, TierFast)`:
   - **`internal_bug`**: Go runtime panic, nil pointer, invariant violation. $\rightarrow$ **IsWardenBug = true**.
   - **`transient_error`**: 429 rate limit, network timeout, connection reset. $\rightarrow$ Suggest backend switch.
   - **`environment_error`**: Missing host tools (`docker`, `make`, `npm` not found on PATH).
   - **`task_failure`**: Test or compiler errors in the code being written by the agent.

---

## 3. Privacy-Safe Redaction & Staging

**Absolute Rule: Zero data is sent to GitHub without explicit user inspection and approval.**

When `IsWardenBug == true`:
1. **Secret Stripping**:
   - OpenAI/Anthropic keys (`sk-...`)
   - Google AI keys (`AIza...`)
   - GitHub tokens (`ghp_...`, `github_pat_...`)
   - Bearer authorization headers
2. **Path Normalization**:
   - `/home/<username>/` $\rightarrow$ `~/`
   - Strips absolute local directory prefixes from stack traces while keeping relative package locations (`internal/planstore/store.go:142`).
3. **Staging**:
   - Staged locally at `~/.warden/crashes/<id>.json`.

---

## 4. User-Approved Bug Reporting Flow

### CLI Command: `warden bug-report <id>`
1. Renders the sanitized preview in terminal:
   - Title: `crash(planstore): nil pointer dereference in UpdatePlanTaskDefinition`
   - Environment: `Warden v9.10.1 (linux/amd64)`
   - Sanitized Stack Trace
2. Prompts: `Submit this bug report to https://github.com/srjn45/warden/issues? [y/N]`
3. If approved:
   - If `gh` CLI is authenticated: executes `gh issue create --repo srjn45/warden ...` and prints live issue URL.
   - Fallback: prints pre-filled clickable GitHub issue link (`https://github.com/srjn45/warden/issues/new?title=...&body=...`).

### TUI Cockpit Integration
- Status bar displays a badge: `[⚠️ Bug Detected: Press B to Review]`.
- Pressing `B` opens an interactive modal preview with `[Submit]` and `[Dismiss]` actions.

---

## 5. TUI Log Viewer (`modeLogs`, Hotkey `l`)

Mirrors the existing `c` (Context & Messages) and `d` (Digest) inspectors in [`internal/tui/control_pane.go`](file:///home/srjn45/dev/warden/internal/tui/control_pane.go):

1. **Hotkey**: `l` in `modeNormal` toggles `modeLogs`.
2. **Viewport**: Reuses `m.vp` inside a framed `titleBox`:
   `titleBox("Logs  (esc / l to close · G bottom · g top)", m.vp.View(), m.w, m.bodyH())`
3. **Scroll & Navigation**:
   - Opens at bottom (`m.vp.GotoBottom()`) to show latest logs.
   - `g`: Scroll to top.
   - `G`: Scroll to bottom.
   - `↑` / `↓` / `pgup` / `pgdn`: Smooth scrolling.
   - `esc` / `l`: Close and return to list.
4. **Log Isolation**:
   - Ensures `setupTUILogging()` catches all log outputs so no raw text ever corrupts the terminal screen.
   - Highlights log levels: `ERROR` (red), `WARN` (yellow), `INFO` (cyan/green).
