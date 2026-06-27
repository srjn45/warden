# Web "TUI" — the cockpit, streamed full-screen into the browser

Status: implemented (Phase 1 + 2; full-screen revision)
Date: 2026-06-27

## Goal

Give the web platform a surface that works **exactly** like the terminal TUI
(`warden tui`) — same three-pane interface, same keyboard shortcuts, the same
real shells and the same Claude Code — so an operator can drive the whole fleet
from a laptop in the identical way they do locally.

It is **not a tab**: it is launched from a highlighted **▢ TUI** button in the top
bar and takes the **whole viewport, edge-to-edge and non-scrollable**, with none
of the dashboard chrome — and it **exits on Ctrl+Q** (from any pane), landing back
on the home view.
It is a desktop/laptop surface (the three-pane cockpit wants width), so it drops
the mobile soft-key bar that the per-agent attach keeps.

## Key realization

We don't reimplement the TUI. Two existing pieces already compose into it:

1. **The TUI cockpit is just a tmux session.** `buildCockpit`
   (`internal/tui/compositor.go`) lays out a detached three-pane session:
   list pane (`warden tui --pane=list`, a bubbletea app), a master shell / `wd
   repl` pane, and a detail pane that opens the selected agent. There is no
   "native rendering" — the interface *is* terminal output.
2. **The web already streams a tmux session into the browser.** `handleAttach`
   (`internal/daemon/attach.go`) bridges `tmux attach-session` ↔ WebSocket over a
   PTY; `AttachTerminal.tsx` renders it with xterm.js (keystrokes, `{cols,rows}`
   resize, `window-size latest`, mobile key bar + touch-wheel).

So the feature is: **point the existing attach bridge at a daemon-owned cockpit
session and render it in a new full-window xterm tab.** Because it streams the
genuine TUI, parity is automatic and permanent — every shortcut
(`enter/n/o/s/a/i/x/D/r/?/j/k/g/G`), the master shell, the detail pane, and every
future TUI change show up on the web with no extra work.

## Why streaming, not a native React reimplementation

The web Cockpit tab already *is* a native fleet UI. A second native "TUI-flavored"
surface would duplicate ~4,700 lines of `internal/tui` in TypeScript and drift
forever. The ask is *exactly* like the TUI, same shortcuts, same way from
anywhere — streaming the real article delivers that; reimplementing only
approximates it.

## Terminal fidelity ("exactly like local")

Nothing is emulated: the master pane runs the operator's real `$SHELL` with their
rc files, and the detail pane runs the real `claude` binary, both in a real PTY
on the host. So shell autosuggestions, tab completion, fzf, and Claude Code's
full TUI work because they *are* those programs — there is no completion logic to
port. The work is making the key/clipboard/terminfo pipe transparent:

- **Modified-key reporting** — Claude Code distinguishes Shift+Enter (newline)
  from Enter (submit). xterm.js is the emulator here, so it must emit the right
  bytes: `PtyTerminal` installs an `attachCustomKeyEventHandler` that maps
  Shift+Enter → `ESC`+`CR` (the Alt+Enter fallback the TUI cockpit already wires
  up via `extended-keys`), and sets `macOptionIsMeta` so ⌥-chords reach the PTY.
  The same handler emits the modifyOtherKeys CSI for **Alt+Arrow**
  (`ESC [ 1 ; 3 A/B/C/D`) so cockpit pane navigation (tmux `bind-key -n
  M-Up/Down/Left/Right`) works — otherwise the browser eats Alt+Left/Right as
  back/forward and `macOptionIsMeta` reroutes the modifier.
- **Browser-reserved chords** — a few combos (`Ctrl+T/W/N`, some `Cmd+…`) the
  browser swallows before JS sees them. This is the only place a browser tab
  can't be byte-for-byte identical to local; running the app as an installed PWA
  (the manifest already ships) reclaims most of them.
- **terminfo / color** — the attach PTY forces `TERM=xterm-256color`; xterm.js
  renders truecolor.
- **Copy / paste** — bracketed paste passes through the real PTY; the
  shift-drag-to-select hint (already documented for the native TUI) applies.
- **Latency** — keystrokes round-trip over the network, the one unavoidable
  physical difference from a local terminal.

## Architecture

```
Browser /tui (fullscreen)   Daemon                          tmux
┌────────────────┐  WS    ┌──────────────────────┐        ┌──────────────────────┐
│ TuiTab          │◄──────►│ handleCockpitAttach  │──PTY──►│ warden-web-cockpit    │
│ → PtyTerminal   │ binary │  ensureWebCockpit()  │ attach │  ├ list pane (bubbletea)│
│   (xterm + fit) │ +ctrl  │  bridgeTmux()        │        │  ├ master shell/repl  │
└────────────────┘        └──────────────────────┘        │  └ detail pane        │
                                                           └──────────────────────┘
```

## Decisions

- **One shared cockpit (`warden-web-cockpit`).** warden is single-user /
  single-daemon, so a shared session *is* the "same session from anywhere"
  continuity that was asked for — open it on a laptop, pick it up on a phone
  mid-navigation. `window-size latest` (already used by attach) lets the
  most-recently-active client drive sizing, so desktop+phone simultaneously
  degrades gracefully instead of fighting. Per-client cockpits can be a later
  opt-in.
- **Master pane is a plain shell** (the `warden tui` default). The `useRepl`
  (`wd repl`) flavor is plumbed through `EnsureWebCockpit` but defaults off,
  reserved for a future config toggle.
- **Shortcut collisions are already solved.** xterm's input is a hidden
  `<textarea>`, and the dashboard's `isTypingTarget` (`lib/shortcuts.ts`) already
  suppresses global single-key shortcuts in textareas — so `n/r/j/k/1–9` flow to
  the TUI, not the web shell, while the terminal has focus.
- **Full-screen route, not a tab.** `tui` is a standalone route (like `agent`),
  removed from `FIXED_ROUTE_KINDS` so it has no tab and no slot in `1-9`/`j-k`
  nav. `Dashboard` early-returns a chrome-less full-viewport `TuiTab` when the
  route is `tui`. Launched from the highlighted top-bar **▢ TUI** button.
- **Ctrl+Q exits.** Ctrl+Q is intercepted in the terminal's
  `attachCustomKeyEventHandler`, `preventDefault`'d, and routed to `onExit()`
  (→ home) instead of the PTY — so it works from **any** pane. We use Ctrl+Q
  rather than a bare `q` so a literal `q` still types into the shells / Claude /
  pagers (full terminal fidelity), and Ctrl+Q isn't an OS-reserved chord the way
  Cmd+Q is.
- **`q` exits the whole TUI (→ home), from the list pane only.** The bloom list
  app quits on `q` and runs `killCockpitCmd`, which tears the whole tmux session
  down (master + detail panes too). Locally that returns you to your shell; on
  the web the daemon's `bridgeTmux` notices the session vanished after the attach
  exits (`tmux has-session` fails) and closes the WebSocket with a private code
  (`wsStatusCockpitEnded` = 4001). `PtyTerminal`'s `onclose` reads that code and,
  in the full-screen TUI, calls `onExit()` → navigate to `/` (an ordinary
  disconnect with no such code still shows the reconnect banner). This is
  inherently **pane-scoped** the way a browser key handler can't be: `q` only
  exits when the *list pane* has focus, because only bloom interprets `q` as
  quit — in the master shell or the Claude detail pane, `q` types normally. The
  cockpit is rebuilt fresh on the next visit (`EnsureWebCockpit` finds no
  session). Ctrl+Q (above) remains the exit-from-anywhere chord. _(This replaced
  an earlier "`q` drops the list pane to a shell" attempt — exposing a terminal
  on `q` was the wrong default; a clean exit-to-home is what's wanted.)_
- **Desktop/laptop only.** The cockpit wants width, so the full-screen TUI sets
  `keyBar={false}` — no mobile soft-key bar. The per-agent attach (still mobile-
  friendly) keeps it.
- **Lazy + idempotent.** The cockpit is built on first attach (`has-session`
  probe, then `buildCockpit`) and reused thereafter; it survives daemon restarts
  as long as the tmux server does, and is rebuilt on the next attach otherwise.

## What changed

Server:
- `internal/tui/compositor.go` — `WebCockpitSession` const + `EnsureWebCockpit`
  (the headless, daemon-callable counterpart to `RunCockpit` — builds, never
  attaches).
- `internal/daemon/attach.go` — extracted `bridgeTmux(w, r, session,
  signalSessionEnd)` shared by per-agent and cockpit attach; added
  `handleCockpitAttach`. When the cockpit attach ends and the session is gone
  (`q` from inside), closes the WS with `wsStatusCockpitEnded` (4001) so the
  browser navigates home; per-agent attach passes `false` (a vanished agent
  session just shows the banner).
- `internal/daemon/api.go` — registered `GET /api/v1/cockpit/attach` next to the
  per-agent attach (hand-wired WS route, same auth group; the `/attach` suffix is
  already in the middleware flush/hijack allowlist).
- `internal/daemon/apidocs/openapi.yaml` — documented the new WS path.

Web:
- `web/src/components/PtyTerminal.tsx` — the shared xterm engine extracted from
  `AttachTerminal`, parameterized by `makeUrl` / `reconnectKey`, with
  `extendedKeys`, `fill`, `onExit`, and `keyBar` options. `onExit` fires on both
  Ctrl+Q (browser-side key) and a `WS_COCKPIT_ENDED` (4001) socket close (the
  server-side `q` exit).
- `web/src/components/AttachTerminal.tsx` — now a thin wrapper over `PtyTerminal`.
- `web/src/components/TuiTab.tsx` — the full-screen cockpit terminal (`onExit`
  + `keyBar={false}`).
- `web/src/components/AttentionBar.tsx` — the highlighted top-bar **▢ TUI**
  launcher (`onOpenTui`).
- `web/src/lib/attach.ts` — `cockpitAttachURL`.
- `web/src/lib/router.ts` — `tui` made a standalone (non-tab) route; parsed/
  round-tripped, kept out of `FIXED_ROUTE_KINDS`.
- `web/src/components/TabBar.tsx` — TUI tab removed.
- `web/src/components/Dashboard.tsx` — early-return full-screen `TuiTab` for the
  `tui` route; top-bar launcher wired.
- `web/src/pages/[...path].astro` — `/tui` deep-link shell (unchanged).
- `web/src/styles/app.css` — `.tui-fullscreen` (fixed, edge-to-edge,
  non-scrollable) + `.tui-launch` (accent button).

## Known limitations / follow-ups

- **Global tmux key tables.** `buildCockpit` sets root-table bindings (`M-Arrow`,
  `M-t`) and a prefix `Enter` binding on the shared tmux server; these can leak to
  other sessions on the same server. Pre-existing TUI behavior; acceptable for a
  single-user daemon, but scoping to the cockpit session is a future cleanup.
- **Mobile.** The full-screen TUI is explicitly a desktop/laptop surface (the
  three-pane layout wants width; no soft-key bar). The per-agent `AttachTerminal`
  already covers "drive one agent" on a phone. A phone-optimized single-pane
  cockpit layout is a possible follow-up.
- **Reconnect.** A dropped socket shows a banner and reconnects on reload; an
  auto-reconnect is a Phase 3 polish item.
