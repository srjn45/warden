# agentctl — scroll-native agent tmux sessions

**Date:** 2026-06-03
**Status:** Approved (design)
**Scope:** Make agent output scrollable in the cockpit TUI.

## Problem

In the cockpit TUI (tmux-composited 3-pane), the detail pane is a *nested*
`tmux attach` to the agent's own session (`internal/tui/list_pane.go:407-416`,
`env -u TMUX tmux attach -t <agent-session>`). Two user-visible symptoms, one
root cause:

1. **Long AI responses can't be read in full** — the detail pane shows the live
   agent session but there is no usable way to scroll back through what the agent
   wrote.
2. **The attached tmux scrolls poorly** — wheel/copy-mode don't behave.

**Root cause:** agent tmux sessions are created with *zero* tmux options
(`internal/lifecycle/lifecycle.go:450, 473, 485` — three `new-session` sites),
so they inherit tmux defaults:

- `mouse` is **off** → the wheel never enters copy-mode. The *cockpit* session
  has `mouse on` (`internal/tui/compositor.go:120`), but for the wheel to scroll
  the agent's history the **inner** (agent) session must also have `mouse on` so
  the outer cockpit can forward the wheel into it. It doesn't, so nothing scrolls.
- `history-limit` is the tmux default **2000 lines** → even when scrolling works,
  long sessions have already lost their oldest output.

## Approach (chosen: "A — scroll-native sessions")

Apply scroll-friendly tmux options to every agent session at creation time. No
architectural change, no change to the nested-attach detail pane, fully
reversible. Rejected alternatives: engineering nested copy-mode keybindings
(approach B — fundamentally limited for the inner session, more moving parts);
de-nesting via `join-pane` or reverting to a capture-pane snapshot (approach C —
invasive / explicitly removed in `d1eb0d9`).

### The two options applied

1. **`mouse on` (per agent session)** — the core fix. `mouse` is a *live*
   session option, so it takes effect immediately for any attached client. This
   makes:
   - the full-screen `a` path (single-level, non-nested) wheel-scroll and
     `Ctrl-b [` copy-mode work natively, and
   - the nested detail-pane wheel scroll work, because the inner session can now
     receive forwarded wheel events.

2. **`history-limit` raised to 50000 (global, only-raise)** — for deep
   scrollback on long sessions. tmux only honors `history-limit` at
   **pane-creation** time, and a session option cannot be set before its first
   window exists, so the only lever is the **global** option, set *before*
   `new-session`. To avoid clobbering a user who has already configured a larger
   value, this is **only-raise**: read the current global `history-limit`; set it
   to 50000 only if the current value is lower; never lower it.

### Ordering constraint

`history-limit` must be ensured **before** `new-session` (pane inherits it at
creation); `mouse on` is set **after** `new-session` (needs the session to
exist). A single helper enforces this ordering.

### Helper consolidation

All three creation sites currently inline `new-session`. Introduce one helper in
`internal/lifecycle/lifecycle.go`:

```
func (l *Lifecycle) newAgentSession(ctx context.Context, runDir, id, cwd string) error {
    // 1. ensure global history-limit >= 50000 (only-raise)
    //    - read:  tmux show-options -g -v history-limit   (parse int; tolerate parse failure → skip raise)
    //    - raise: tmux set-option -g history-limit 50000  (only if current < 50000)
    // 2. tmux new-session -d -s <id> -c <cwd>             (run in runDir, preserving current behavior)
    // 3. tmux set-option -t <id> mouse on
    // returns first error encountered; new-session errors are fatal as today,
    // option-setting errors are non-fatal (log/ignore) so a tmux quirk never
    // blocks a spawn.
}
```

Replace the three inline `new-session` calls:

- `lifecycle.go:450` — prompt-mode: `newAgentSession(ctx, "", id, req.Cwd)`
- `lifecycle.go:473` — typed/managed: `newAgentSession(ctx, req.Repo, id, workdir)`
- `lifecycle.go:485` — `resumeInTmux` (Restore/Adopt): `newAgentSession(ctx, "", id, cwd)`

The subsequent `send-keys` (claude launch/resume) calls are unchanged.

**Decision — option-set failures are non-fatal:** `mouse`/`history-limit`
failures must not break a spawn that otherwise succeeds; the `new-session`
failure stays fatal exactly as today.

## Behavior after the fix

In cockpit:
- **Wheel** over the detail pane scrolls the agent's output.
- Press **`a`** to full-screen the agent → wheel-scroll or `Ctrl-b [` copy-mode
  natively over a deep (50k-line) scrollback; **`Ctrl-b Enter`** to return.

**Tradeoff (accepted):** with `mouse on`, selecting text inside claude with the
mouse now requires holding **Shift** to bypass tmux — the standard tmux mouse-on
tradeoff, already true of the cockpit session itself.

**Global side-effect (accepted):** the user's global tmux `history-limit` may be
raised to 50000. Only-raise means it is never lowered; for users who already set
it higher it is left untouched.

## Testing

`internal/lifecycle` tests drive a fake command runner that records every tmux
invocation. TDD:

1. **Failing test first** — assert each of the three spawn/restore paths issues
   `set-option -t <id> mouse on` after `new-session`.
2. Assert `history-limit` is **ensured before** `new-session` in the recorded
   command order (show-options then, when current < 50000, set-option -g).
3. **Only-raise:** when the fake runner reports current global `history-limit`
   ≥ 50000, assert no `set-option -g history-limit` is issued.
4. **Non-fatal options:** when the fake runner errors on a `set-option`, assert
   the spawn still returns success (session object returned, no error).
5. Existing `new-session` / `send-keys` ordering assertions continue to pass.

## Out of scope (YAGNI)

- Classic-mode viewport fixes (separate concern; user uses cockpit).
- Nested-tmux keyboard copy-mode keybindings / prefix passthrough (approach B).
- Making `history-limit` value configurable (hard-code 50000; revisit if asked).
- Setting `mode-keys` (respect the user's tmux preference).

## Operational note

The change lives in the daemon (lifecycle). A **running daemon must be rebuilt
and restarted** for new spawns to pick up the options (`make release`/`install`).
Existing already-running agent sessions won't gain deep scrollback retroactively
(their panes were created with the old history-limit), but a restart of the
daemon + new agents will. `mouse on` could be applied to existing sessions
manually but that is out of scope.
