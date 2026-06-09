# Cockpit Master-Pane Shell Toggle Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a prefix-less `M-t` key to the warden cockpit that toggles the bottom-left master pane between the interactive Claude session and a shell, killing neither.

**Architecture:** A pure helper builds an idempotent `sh` script; `buildCockpit` binds it to `M-t` via `tmux run-shell`. On each press the script lazily creates a `$SHELL` pane in a hidden holding window (tracked by the session user-option `@warden_shell_pane`), then `swap-pane`s it with the master Claude pane. Because the same pane pair is always swapped, one binding toggles forever; because the shell is tracked by a re-read option (and kept alive with `remain-on-exit`), the toggle self-heals after the shell is exited.

**Tech Stack:** Go, tmux ≥ 3.1, `testify/require`, `lifecycle.FakeRunner` for unit tests.

**Spec:** `docs/superpowers/specs/2026-06-09-tui-master-pane-shell-toggle-design.md`

---

## File Structure

- `internal/tui/compositor.go` — add the pure `shellToggleScript` helper and the `M-t` `bind-key` call inside `buildCockpit`.
- `internal/tui/compositor_test.go` — unit test for `shellToggleScript`; update `TestBuildCockpitSequence` for the new binding.
- `README.md` — add `M-t` to the cockpit key table.
- `docs/USAGE.md` — document the toggle in the bottom-left master section.

---

## Task 1: `shellToggleScript` pure helper

**Files:**
- Modify: `internal/tui/compositor.go` (add helper near `listPaneCmd`/`detailPlaceholderCmd`, ~line 45)
- Test: `internal/tui/compositor_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/tui/compositor_test.go`:

```go
func TestShellToggleScript(t *testing.T) {
	s := shellToggleScript("S", "%1", "/work")
	// Tracks the shell pane in a session user-option so the toggle survives exit.
	require.Contains(t, s, "@warden_shell_pane")
	// Lazily creates the shell in a hidden holding window with the user's $SHELL.
	require.Contains(t, s, "new-window -d -t S -n warden-shell -c '/work'")
	require.Contains(t, s, `"${SHELL:-/bin/sh}"`)
	// Exited shells are kept as [exited] then respawned, not orphaned.
	require.Contains(t, s, "remain-on-exit on")
	require.Contains(t, s, "respawn-pane")
	// Swaps the shell with the master pane and focuses whatever lands in the slot.
	require.Contains(t, s, `swap-pane -s "$sp" -t %1`)
	require.Contains(t, s, "select-pane -t '{bottom-left}'")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/tui/ -run TestShellToggleScript -v`
Expected: FAIL — `undefined: shellToggleScript`

- [ ] **Step 3: Write minimal implementation**

Add to `internal/tui/compositor.go` (after `detailPlaceholderCmd`, around line 45). `fmt` and `strings` are already imported:

```go
// shellToggleScript returns the sh command bound to M-t in the cockpit. On each
// press it surfaces a shell in the bottom-left master slot, or returns to the
// master Claude, by swapping the two panes — neither process is killed. The
// shell is created lazily on the first toggle in a hidden holding window and
// tracked by the session user-option @warden_shell_pane, so the toggle survives
// the user exiting the shell (kept as [exited] via remain-on-exit, then
// respawned). session is the cockpit tmux session, masterPane the master
// Claude pane's stable id, and cwd the directory the shell starts in.
func shellToggleScript(session, masterPane, cwd string) string {
	c := shquote(cwd)
	return fmt.Sprintf(`sp=$(tmux show-options -v -t %[1]s @warden_shell_pane 2>/dev/null)
if [ -z "$sp" ] || ! tmux list-panes -s -t %[1]s -F '#{pane_id}' | grep -qx "$sp"; then
  sp=$(tmux new-window -d -t %[1]s -n warden-shell -c %[3]s -P -F '#{pane_id}' "${SHELL:-/bin/sh}")
  tmux set-option -t %[1]s @warden_shell_pane "$sp"
  tmux set-option -p -t "$sp" remain-on-exit on
elif tmux list-panes -s -t %[1]s -F '#{pane_id} #{pane_dead}' | grep -qx "$sp 1"; then
  tmux respawn-pane -t "$sp" -c %[3]s "${SHELL:-/bin/sh}"
fi
tmux swap-pane -s "$sp" -t %[2]s
tmux select-pane -t '{bottom-left}'`, session, masterPane, c)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/tui/ -run TestShellToggleScript -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/tui/compositor.go internal/tui/compositor_test.go
git commit -m "feat(tui): add shellToggleScript helper for cockpit shell toggle"
```

---

## Task 2: Bind `M-t` in `buildCockpit`

**Files:**
- Modify: `internal/tui/compositor.go:104-112` (insert after the `M-Arrow` bind-key loop, before the focus `select-pane`)
- Test: `internal/tui/compositor_test.go` (`TestBuildCockpitSequence`)

- [ ] **Step 1: Update the failing test**

In `internal/tui/compositor_test.go`, change `TestBuildCockpitSequence` to expect 15 calls and the new `M-t` binding at index 12. Change the length assertion:

```go
	require.Len(t, fr.Calls, 15, "unexpected number of tmux calls")
```

Then, immediately after the existing `M-Down` assertion (`fr.Calls[11]`), insert the new binding assertion and renumber the final two:

```go
	require.Equal(t, []string{"tmux", "bind-key", "-n", "M-Down", "select-pane", "-D"}, fr.Calls[11].Argv)
	// M-t toggles the bottom-left master pane between Claude and a shell.
	require.Equal(t, []string{"tmux", "bind-key", "-n", "M-t", "run-shell", "-b", shellToggleScript("S", "%1", "/work")}, fr.Calls[12].Argv)
	require.Equal(t, []string{"tmux", "select-pane", "-t", "%2"}, fr.Calls[13].Argv)
	// Return-to-dashboard binding for the full-screen attach path (`a`).
	require.Equal(t, []string{"tmux", "bind-key", "Enter", "switch-client", "-l"}, fr.Calls[14].Argv)
```

(Delete the old `fr.Calls[12]` select-pane and `fr.Calls[13]` Enter assertions — they are replaced by the index-13 and index-14 lines above.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/tui/ -run TestBuildCockpitSequence -v`
Expected: FAIL — `Len` mismatch (got 14, want 15) and/or index-12 argv mismatch.

- [ ] **Step 3: Write minimal implementation**

In `internal/tui/compositor.go`, insert this block in `buildCockpit` directly after the `M-Arrow` loop's closing `}` (currently ends at line 108) and before the `// 6. Focus the list pane.` comment (line 109):

```go
	// M-t toggles the bottom-left master pane between Claude and a shell, swapping
	// them without killing either (see shellToggleScript). Best-effort parity with
	// the M-Arrow bindings above.
	if out, err := run.Run(ctx, "", "tmux", "bind-key", "-n", "M-t", "run-shell", "-b", shellToggleScript(o.session, masterID, o.masterCwd)); err != nil {
		return fmt.Errorf("tmux bind-key M-t: %w: %s", err, out)
	}
```

- [ ] **Step 4: Run the package tests to verify they pass**

Run: `go test ./internal/tui/ -v -run 'TestBuildCockpitSequence|TestShellToggleScript'`
Expected: PASS for both.

- [ ] **Step 5: Run the full TUI package + vet to confirm no regressions**

Run: `go test ./internal/tui/... && go vet ./internal/tui/...`
Expected: ok, no failures.

- [ ] **Step 6: Commit**

```bash
git add internal/tui/compositor.go internal/tui/compositor_test.go
git commit -m "feat(tui): bind M-t to toggle cockpit master pane between Claude and shell"
```

---

## Task 3: Document `M-t` in README and USAGE

**Files:**
- Modify: `README.md:251` (cockpit key table) and `README.md:253` (focus-nav line)
- Modify: `docs/USAGE.md` (bottom-left master section, ~line 414-431)

- [ ] **Step 1: Add the key-table row in README**

In `README.md`, insert a new row into the cockpit key table immediately before the `q` row (line 251):

```markdown
| `Alt+t` | Toggle the bottom-left master pane between Claude and a shell (both stay alive) |
| `q` | Quit and tear down the cockpit |
```

- [ ] **Step 2: Mention it on the focus-nav line**

In `README.md`, replace the focus line (currently line 253):

```markdown
Move focus between panes with **Alt+←/→/↑/↓** (no tmux prefix); toggle the bottom-left master pane between Claude and a shell with **Alt+t**. See [docs/USAGE.md §7](docs/USAGE.md) for the full cockpit guide and caveats around nested tmux.
```

- [ ] **Step 3: Document the toggle in USAGE.md**

In `docs/USAGE.md`, in the "Bottom-left — master Claude" subsection (after the existing paragraph around line 415-424), add:

```markdown
Press **Alt+t** to toggle this slot between the master Claude session and a
shell. The shell is created on first use and both keep running across toggles —
switching back and forth never loses the conversation or the shell's scrollback.
Exit the shell (`exit` / Ctrl-D) and the next **Alt+t** starts a fresh one.
```

- [ ] **Step 4: Verify docs reference the same key**

Run: `grep -rn "Alt+t" README.md docs/USAGE.md`
Expected: matches in all three spots (key table, focus line, USAGE section).

- [ ] **Step 5: Commit**

```bash
git add README.md docs/USAGE.md
git commit -m "docs: document cockpit Alt+t master-pane shell toggle"
```

---

## Task 4: Build and manual smoke test

**Files:** none (verification only)

- [ ] **Step 1: Build the binary**

Run: `go build ./...`
Expected: builds clean, no errors.

- [ ] **Step 2: Run the full test suite for the touched package**

Run: `go test ./internal/tui/...`
Expected: ok.

- [ ] **Step 3: Reinstall so the running cockpit uses the new binary**

The cockpit is launched by the installed `warden` binary, so it must be replaced.

Run: `make install`
Expected: build + install succeeds. (No daemon restart needed — this change is cockpit-only and does not touch the daemon.)

- [ ] **Step 4: Manual smoke test the toggle**

From a plain terminal (not inside tmux):

1. Run `warden tui` to open the cockpit. Confirm the bottom-left shows the master Claude session.
2. Press **Alt+t** → a shell appears in the bottom-left slot, same dimensions; focus is in the shell. Type `echo hello` and confirm it runs.
3. Press **Alt+t** → the master Claude returns with its prior output intact; focus is in it.
4. Press **Alt+t** again → the *same* shell returns with `echo hello` still in its scrollback (not a fresh shell).
5. With the shell showing, type `exit`. The slot shows `[exited]` (it does not collapse).
6. Press **Alt+t** → focus returns to Claude (a fresh shell has been prepared in the background).
7. Press **Alt+t** once more → a fresh shell appears in the slot.
8. Press **`q`** → the whole cockpit tears down. Confirm no leftover windows/sessions:

Run: `tmux list-sessions 2>/dev/null | grep warden-tui; echo "exit=$?"`
Expected: no `warden-tui-*` session remains.

- [ ] **Step 5: Confirm completion**

All steps above pass with the observed behavior matching expectations. No commit (verification only).

---

## Notes for the implementer

- **tmux features used:** `swap-pane -s/-t` works across windows in the same session by pane id; `select-pane -t '{bottom-left}'` targets the bottom-left pane of the current window positionally (direction-agnostic); `remain-on-exit on` keeps an exited pane as `[exited]` so it can be respawned. All require tmux ≥ 3.1, which the cockpit already mandates.
- **Why a session user-option, not a baked pane id:** `swap-pane` physically moves panes between the main and holding windows, and the shell may be exited. Re-reading `@warden_shell_pane` each press (and recreating/respawning when stale) keeps the single static binding correct indefinitely.
- **Scope:** cockpit only. Classic/standalone `RunListPane` has no master pane and is untouched.
