# TUI cockpit: toggle the master pane between Claude and a shell

Date: 2026-06-09

## Problem

The cockpit's bottom-left "master" pane runs a bare interactive `claude` session
(`internal/tui/compositor.go:77`). While orchestrating agents the user sometimes
wants raw terminal access in that slot without quitting the cockpit, then wants
to return to the master Claude session with its conversation intact. Today there
is no way to do this — the slot is fixed to `claude` for the cockpit's lifetime.

## Goal

A single prefix-less keypress toggles the bottom-left slot between the master
Claude session and a shell. Neither process is ever killed by toggling: both
keep running with full scrollback, so switching back and forth is lossless.

## Scope

- Cockpit only (`buildCockpit` in `internal/tui/compositor.go`).
- The classic/standalone list mode (`RunListPane`) has no master pane and is
  not touched.
- Out of scope: toggling any other pane; persisting shell history across
  separate cockpit launches; a configurable key (v1 hardcodes `M-t`).

## Decisions

- **Toggle key:** `M-t` (Alt+t), prefix-less, consistent with the existing
  `M-Left/Right/Up/Down` pane navigation bound in `buildCockpit`.
- **Shell lifecycle:** lazy — the shell pane is created on the first `M-t`, not
  at cockpit build time.
- **Shell:** `$SHELL`, falling back to `/bin/sh` when unset, started in
  `masterCwd` (the launching shell's directory — the same cwd as the master
  Claude pane).
- **First-toggle hint message:** skipped for v1.

## Mechanism — guarded swap via a holding window

The master Claude pane is created exactly as today; its stable pane id
(`masterID`, already captured at `compositor.go:77`) is baked into the toggle
binding.

`M-t` is bound prefix-less to a small idempotent `sh` script, invoked via
`tmux run-shell -b`. On each press the script:

1. Reads the shell pane id from a tmux **session user-option**
   `@warden_shell_pane` (empty / unset on the first toggle).
2. If that pane id is missing or no longer alive (first toggle, or the user
   exited the shell with `exit`/Ctrl-D), it creates a hidden holding window in
   the cockpit session running `$SHELL` in `masterCwd`, captures the new pane id
   with `-P -F '#{pane_id}'`, and stores it back into `@warden_shell_pane`.
   This is the lazy creation — nothing spawns until the first `M-t`.
3. Runs `swap-pane -s <shellPane> -t <masterPane>`, swapping those two specific
   panes wherever they currently sit (tmux `swap-pane` works across windows by
   pane id).
4. Runs `select-pane -t '{bottom-left}'` so focus lands on whatever now occupies
   the slot.

### Why these choices

- **Same id pair every time → one static-shaped binding toggles forever.**
  `swap-pane -s shell -t master` swaps the same two panes regardless of which
  window each is in, so pressing `M-t` repeatedly alternates them. `swap-pane`
  preserves each slot's geometry, so the bottom-left dimensions never change.
- **Session user-option, not a baked-once id → survives shell exit.** If the
  shell were referenced by an id baked into the binding at creation, exiting the
  shell would leave a dangling reference and a broken toggle. Reading
  `@warden_shell_pane` each press and re-bootstrapping when stale makes the
  toggle self-healing: after `exit`, the next `M-t` recreates the shell.
- **`{bottom-left}` for focus → direction-agnostic, no state.** The pane id in
  the slot changes on every toggle, but the slot is always the cockpit window's
  bottom-left, so a positional `select-pane` target focuses the surfaced pane
  without tracking direction.

### Lifecycle of the master Claude pane

Unchanged. Exiting the master Claude itself (Ctrl-D) closes its pane as it does
today; that is a "cockpit is done" signal and out of scope. After that there is
nothing to toggle to, which is acceptable.

## Teardown

No new teardown. The holding window lives inside the cockpit tmux session, so it
is reaped by the existing paths:

- `q` → `killCockpitCmd` → `tmux kill-session` (`internal/tui/list_pane.go`).
- `cleanStaleCockpits` (`compositor.go:141`) on the next launch for cockpits
  orphaned by a bare detach.

## Discoverability

Document `M-t` in the README cockpit section, alongside the existing `M-Arrow`
navigation (which is likewise a tmux-level binding not shown in the on-screen
list-pane footer). No on-screen legend change for v1.

## Testing

- **Unit (fake `lifecycle.Runner`, mirroring existing `compositor` tests):**
  assert that `buildCockpit` registers the `M-t` `bind-key`, and that the
  bootstrap script string contains the expected pieces — the
  `@warden_shell_pane` read, the conditional `new-window` creating the shell,
  the `swap-pane -s ... -t <masterID>`, and the `select-pane -t {bottom-left}`.
  Assert the master pane is still created with the bare `claude` command
  (no regression).
- **Manual smoke (tmux runtime, as the rest of the cockpit is verified):**
  launch the cockpit; `M-t` surfaces a shell in the bottom-left; type in it;
  `M-t` returns to the master Claude with its prior output intact; `M-t` again
  returns to the same shell with its scrollback intact; `exit` the shell, then
  `M-t` recreates a fresh shell; `q` tears the whole cockpit down with no orphan
  windows or sessions left behind.

## Files

- `internal/tui/compositor.go` — add the `M-t` binding + bootstrap script in
  `buildCockpit`; thread `masterCwd`/`masterID` into the script.
- `internal/tui/compositor_test.go` — unit assertions above.
- `README` (cockpit keys section) — document `M-t`.
