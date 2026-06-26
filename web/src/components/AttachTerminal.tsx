import { useEffect, useRef, useState } from 'react';
import { Terminal as XTerm } from '@xterm/xterm';
import { FitAddon } from '@xterm/addon-fit';
import '@xterm/xterm/css/xterm.css';
import { attachURL, resizeMessage } from '../lib/attach';

// AttachTerminal is a fully interactive terminal bridged to a real `tmux attach`
// over a WebSocket: keystrokes (binary frames) go to the agent, PTY output
// (binary frames) is rendered, and fit/resize is sent as a text control frame.
//
// Mobile: the agent's tmux session is attached in its alternate screen with
// `mouse on`, so xterm's own scrollback is always empty — the real history lives
// in tmux copy-mode (or the agent's full-screen TUI), both of which scroll on
// mouse-wheel events. A desktop wheel sends those; a phone doesn't translate a
// touch-drag into one, so we synthesise the wheel sequences from the swipe. A key
// bar also sends the control keys (Esc/Tab/Ctrl-C/arrows) a soft keyboard lacks.

// wheelSeq is one SGR mouse-wheel notch (button 64 = up, 65 = down) at a 1-based
// cell coordinate — exactly what tmux (mouse on) and full-screen TUIs expect from
// a real wheel, so it drives their native scrollback / copy-mode.
const wheelSeq = (up: boolean, col: number, row: number) =>
  `\x1b[<${up ? 64 : 65};${col};${row}M`;

export default function AttachTerminal({ id }: { id: string }) {
  const hostRef = useRef<HTMLDivElement>(null);
  const termRef = useRef<XTerm | null>(null);
  // send writes raw bytes to the agent (same path as keystrokes); held in a ref
  // so the key bar can reach the live socket without re-rendering.
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
    });
    const fit = new FitAddon();
    term.loadAddon(fit);
    term.open(host);
    fit.fit();
    termRef.current = term;

    const ws = new WebSocket(attachURL(window.location, id));
    ws.binaryType = 'arraybuffer';

    const send = (s: string) => {
      if (ws.readyState === WebSocket.OPEN) ws.send(new TextEncoder().encode(s));
    };
    sendRef.current = send;

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
    // send them to the agent (tmux / its TUI scrolls in response — see wheelSeq).
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
  }, [id]);

  // A key bar press sends its sequence, then refocuses the terminal so the soft
  // keyboard stays up. "Bottom" walks the view back to the live tail with a burst
  // of wheel-down notches (which also drops tmux out of copy-mode at the bottom).
  const key = (seq: string) => () => { sendRef.current(seq); termRef.current?.focus(); };
  const toBottom = () => { sendRef.current(wheelSeq(false, 1, 1).repeat(60)); termRef.current?.focus(); };

  return (
    <div className="xterm-wrap">
      {closed && (
        <div style={{ color: '#cf222e', fontSize: '.75rem', padding: '.15rem .4rem' }}>
          disconnected — the agent may have ended
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
