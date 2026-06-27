import { cockpitAttachURL } from '../lib/attach';
import PtyTerminal from './PtyTerminal';

// TuiTab is the web "TUI": a full-window terminal attached to the daemon-owned
// three-pane cockpit (agent list + master shell/REPL + detail pane). It is the
// literal `warden tui`, streamed into the browser over the same PTY↔WS bridge as
// a per-agent attach — so every TUI keybinding (enter/n/o/s/a/i/x/D/r/?/j/k…),
// the master shell, and Claude Code in the detail pane behave exactly as they do
// locally, from any device. `extendedKeys` adds the Shift+Enter handling Claude
// Code needs; `fill` makes the terminal own the whole tab.
//
// Focus note: while this terminal has focus its hidden <textarea> is a typing
// target, so the dashboard's global single-key shortcuts (n/r/j/k/1–9) stay
// dormant and flow to the TUI instead — click a tab in the bar (mouse) to leave.
export default function TuiTab() {
  return (
    <div className="tui-tab">
      <PtyTerminal
        reconnectKey="cockpit"
        makeUrl={cockpitAttachURL}
        extendedKeys
        fill
        disconnectedMsg="cockpit disconnected — reload the page to reconnect"
      />
    </div>
  );
}
