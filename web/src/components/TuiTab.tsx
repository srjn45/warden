import { cockpitAttachURL } from '../lib/attach';
import PtyTerminal from './PtyTerminal';

// TuiTab is the web "TUI": a full-screen terminal attached to the daemon-owned
// three-pane cockpit (agent list + master shell/REPL + detail pane). It is the
// literal `warden tui`, streamed into the browser over the same PTY↔WS bridge as
// a per-agent attach — so every TUI keybinding (enter/n/o/s/a/i/x/D/r/?/j/k…),
// the master shell, and Claude Code in the detail pane behave exactly as they do
// locally. It is a desktop/laptop surface (the three-pane cockpit wants width),
// so it drops the mobile key bar (`keyBar={false}`).
//
// It is launched from the top-bar ▢ TUI button (not a tab) and takes the whole
// viewport, edge-to-edge and non-scrollable. `extendedKeys` adds the Shift+Enter
// / Alt+Arrow handling the cockpit needs; `fill` makes the terminal own the
// frame; `onExit` is bound to Ctrl+Q, which leaves the cockpit (from any pane)
// and lands back on the dashboard home.
//
// Focus note: while this terminal has focus its hidden <textarea> is a typing
// target, so the dashboard's global single-key shortcuts (n/r/j/k/1–9) stay
// dormant and flow to the TUI instead — press `q` to leave.
export default function TuiTab({ onExit }: { onExit: () => void }) {
  return (
    <div className="tui-fullscreen">
      <PtyTerminal
        reconnectKey="cockpit"
        makeUrl={cockpitAttachURL}
        extendedKeys
        fill
        onExit={onExit}
        keyBar={false}
        disconnectedMsg="cockpit disconnected — reload the page to reconnect"
      />
    </div>
  );
}
