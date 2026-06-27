import { useEffect, useRef, useState } from 'react';
import { Terminal as XTerm } from '@xterm/xterm';
import { FitAddon } from '@xterm/addon-fit';
import '@xterm/xterm/css/xterm.css';
import { resizeMessage } from '../lib/attach';

// PtyTerminal is a fully interactive terminal bridged to a real `tmux attach`
// over a WebSocket: keystrokes (binary frames) go to the PTY, PTY output (binary
// frames) is rendered, and fit/resize is sent as a text control frame. It is the
// shared engine behind both the per-agent attach (AttachTerminal) and the web
// "TUI" tab (TuiTab) — the only difference is which tmux session the daemon
// bridges (a single agent vs the shared three-pane cockpit), expressed via
// `makeUrl`.
//
// Because the shell and Claude Code run server-side in a real PTY, everything a
// local terminal offers — tab completion, shell autosuggestions, Claude's full
// TUI — works as-is: nothing is emulated, only piped. `extendedKeys` adds the
// browser-side key handling those programs need but the browser would otherwise
// swallow or flatten (e.g. Shift+Enter → newline in Claude Code).
//
// Mobile: the session is attached in its alternate screen with `mouse on`, so
// xterm's own scrollback is always empty — the real history lives in tmux
// copy-mode (or a full-screen TUI), both of which scroll on mouse-wheel events.
// A desktop wheel sends those; a phone doesn't translate a touch-drag into one,
// so we synthesise the wheel sequences from the swipe. A key bar also sends the
// control keys (Esc/Tab/Ctrl-C/arrows) a soft keyboard lacks.

// wheelSeq is one SGR mouse-wheel notch (button 64 = up, 65 = down) at a 1-based
// cell coordinate — exactly what tmux (mouse on) and full-screen TUIs expect from
// a real wheel, so it drives their native scrollback / copy-mode.
const wheelSeq = (up: boolean, col: number, row: number) =>
  `\x1b[<${up ? 64 : 65};${col};${row}M`;

export default function PtyTerminal({
  makeUrl,
  reconnectKey,
  extendedKeys = false,
  fill = false,
  disconnectedMsg = 'disconnected — the agent may have ended',
}: {
  // makeUrl builds the WebSocket URL from the live location; called inside the
  // connect effect (client-only, where window.location is real).
  makeUrl: (loc: { protocol: string; host: string }) => string;
  // reconnectKey is the stable identity that drives the connect effect — change
  // it to reconnect (e.g. an agent id); the cockpit uses a constant.
  reconnectKey: string;
  extendedKeys?: boolean;
  fill?: boolean;
  disconnectedMsg?: string;
}) {
  const hostRef = useRef<HTMLDivElement>(null);
  const termRef = useRef<XTerm | null>(null);
  // send writes raw bytes to the PTY (same path as keystrokes); held in a ref so
  // the key bar can reach the live socket without re-rendering.
  const sendRef = useRef<(s: string) => void>(() => {});
  const [closed, setClosed] = useState(false);

  useEffect(() => {
    const host = hostRef.current;
    if (!host) return;
    setClosed(false);

    const term = new XTerm({
      fontSize: 12,
      fontFamily: 'ui-monospace, SFMono-Regular, Menlo, monospace',
      theme: { background: '#0b0b0b', foreground: '#d6d6d6' },
      scrollback: 5000,
      cursorBlink: true,
      // Treat the Mac ⌥ key as Meta so Alt-chords (and Claude's Alt+Enter
      // fallback) reach the PTY rather than inserting accented characters.
      macOptionIsMeta: true,
    });
    const fit = new FitAddon();
    term.loadAddon(fit);
    term.open(host);
    fit.fit();
    termRef.current = term;

    const ws = new WebSocket(makeUrl(window.location));
    ws.binaryType = 'arraybuffer';

    const send = (s: string) => {
      if (ws.readyState === WebSocket.OPEN) ws.send(new TextEncoder().encode(s));
    };
    sendRef.current = send;

    // Extended-key handling: the real programs server-side want modified keys the
    // browser flattens. Shift+Enter is the load-bearing one — Claude Code reads it
    // as "newline, don't submit"; we send the ESC+CR (Alt+Enter) sequence the TUI
    // cockpit already wires up as the Shift+Enter fallback.
    if (extendedKeys) {
      // Alt+Arrow moves between cockpit panes (tmux `bind-key -n M-Up/Down/
      // Left/Right`). The browser would otherwise eat Alt+Left/Right as
      // back/forward navigation, and `macOptionIsMeta` reroutes the modifier, so
      // neither reliably reaches the PTY. Emit the modifyOtherKeys CSI ourselves
      // (modifier 3 = Alt, which tmux reads as M-) and swallow the browser event.
      const altArrow: Record<string, string> = {
        ArrowUp: '\x1b[1;3A',
        ArrowDown: '\x1b[1;3B',
        ArrowRight: '\x1b[1;3C',
        ArrowLeft: '\x1b[1;3D',
      };
      term.attachCustomKeyEventHandler((e) => {
        if (e.type !== 'keydown') return true;
        if (e.key === 'Enter' && e.shiftKey && !e.ctrlKey && !e.metaKey) {
          e.preventDefault();
          send('\x1b\r');
          return false;
        }
        if (e.altKey && !e.ctrlKey && !e.metaKey && !e.shiftKey && altArrow[e.key]) {
          e.preventDefault();
          send(altArrow[e.key]);
          return false;
        }
        return true;
      });
    }

    const sendResize = () => {
      try { fit.fit(); } catch { /* host detached */ }
      if (ws.readyState === WebSocket.OPEN) ws.send(resizeMessage(term.cols, term.rows));
    };

    ws.onopen = () => { sendResize(); term.focus(); };
    ws.onmessage = (e) => {
      if (e.data instanceof ArrayBuffer) term.write(new Uint8Array(e.data));
    };
    ws.onclose = () => setClosed(true);
    ws.onerror = () => setClosed(true);

    const dataSub = term.onData((d) => send(d));

    const ro = new ResizeObserver(() => sendResize());
    ro.observe(host);

    const onClick = () => term.focus();
    host.addEventListener('click', onClick);

    // Touch-drag → mouse wheel: convert the vertical swipe into wheel notches and
    // send them to the PTY (tmux / its TUI scrolls in response — see wheelSeq).
    // We anchor the wheel at the cell under the finger so it targets the right
    // pane. preventDefault stops the page from scrolling and suppresses the
    // synthetic click, so a drag doesn't pop the keyboard.
    const cellH = () => host.clientHeight / Math.max(term.rows, 1);
    const cellW = () => host.clientWidth / Math.max(term.cols, 1);
    const clamp = (n: number, hi: number) => Math.min(Math.max(n, 1), hi);
    let lastY = 0;
    let acc = 0;
    let col = 1;
    let row = 1;
    const onTouchStart = (e: TouchEvent) => {
      const t = e.touches[0];
      const r = host.getBoundingClientRect();
      lastY = t.clientY;
      acc = 0;
      col = clamp(Math.floor((t.clientX - r.left) / cellW()) + 1, term.cols);
      row = clamp(Math.floor((t.clientY - r.top) / cellH()) + 1, term.rows);
    };
    const onTouchMove = (e: TouchEvent) => {
      const y = e.touches[0].clientY;
      acc += y - lastY; // finger moving down (revealing older lines) is positive
      lastY = y;
      const step = cellH() * 2; // one wheel notch per ~2 rows of finger travel
      while (step > 0 && Math.abs(acc) >= step) {
        const up = acc > 0;
        send(wheelSeq(up, col, row));
        acc += up ? -step : step;
      }
      e.preventDefault();
    };
    host.addEventListener('touchstart', onTouchStart, { passive: true });
    host.addEventListener('touchmove', onTouchMove, { passive: false });

    return () => {
      host.removeEventListener('click', onClick);
      host.removeEventListener('touchstart', onTouchStart);
      host.removeEventListener('touchmove', onTouchMove);
      ro.disconnect();
      dataSub.dispose();
      ws.close();
      term.dispose();
      termRef.current = null;
    };
    // makeUrl/extendedKeys are fixed per mount point; reconnectKey alone drives
    // re-arming (reconnect on a new target).
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [reconnectKey]);

  // A key bar press sends its sequence, then refocuses the terminal so the soft
  // keyboard stays up. "Bottom" walks the view back to the live tail with a burst
  // of wheel-down notches (which also drops tmux out of copy-mode at the bottom).
  const key = (seq: string) => () => { sendRef.current(seq); termRef.current?.focus(); };
  const toBottom = () => { sendRef.current(wheelSeq(false, 1, 1).repeat(60)); termRef.current?.focus(); };

  return (
    <div className={`xterm-wrap${fill ? ' xterm-fill' : ''}`}>
      {closed && (
        <div style={{ color: '#cf222e', fontSize: '.75rem', padding: '.15rem .4rem' }}>
          {disconnectedMsg}
        </div>
      )}
      <div className="xterm-host" ref={hostRef} tabIndex={0} />
      <div className="term-keybar" role="toolbar" aria-label="Terminal keys">
        <button type="button" onClick={key('\x1b')}>Esc</button>
        <button type="button" onClick={key('\t')}>Tab</button>
        <button type="button" onClick={key('\x03')}>Ctrl-C</button>
        <button type="button" onClick={key('\x1b[A')} aria-label="Up">↑</button>
        <button type="button" onClick={key('\x1b[B')} aria-label="Down">↓</button>
        <button type="button" className="term-keybar-bottom" onClick={toBottom}>⤓ Bottom</button>
      </div>
    </div>
  );
}
