import { useEffect, useRef, useState } from 'react';
import { Terminal as XTerm } from '@xterm/xterm';
import { FitAddon } from '@xterm/addon-fit';
import '@xterm/xterm/css/xterm.css';
import { attachURL, resizeMessage } from '../lib/attach';

// AttachTerminal is a fully interactive terminal bridged to a real `tmux attach`
// over a WebSocket: keystrokes (binary frames) go to the agent, PTY output
// (binary frames) is rendered, and fit/resize is sent as a text control frame.
//
// Mobile: xterm v5's built-in touch scrolling is unreliable on phones and a phone
// keyboard has no Esc/Tab/Ctrl/arrow keys, so two things make the terminal usable
// there — we translate vertical swipes into scrollback movement ourselves, and a
// key bar sends the control sequences a soft keyboard can't.
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

    // Touch-drag the scrollback: convert the vertical swipe distance into whole
    // rows and feed it to term.scrollLines, re-anchoring on the remainder so the
    // motion tracks the finger. preventDefault stops the page from scrolling (and
    // suppresses the synthetic click, so a drag doesn't pop the keyboard).
    let lastY = 0;
    let acc = 0;
    const rowHeight = () => host.clientHeight / Math.max(term.rows, 1);
    const onTouchStart = (e: TouchEvent) => { lastY = e.touches[0].clientY; acc = 0; };
    const onTouchMove = (e: TouchEvent) => {
      const y = e.touches[0].clientY;
      acc += lastY - y;
      lastY = y;
      const h = rowHeight();
      const lines = Math.trunc(acc / h);
      if (lines !== 0) { term.scrollLines(lines); acc -= lines * h; }
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
  // keyboard stays up. "Bottom" jumps past the scrollback to the live prompt.
  const key = (seq: string) => () => { sendRef.current(seq); termRef.current?.focus(); };
  const toBottom = () => { termRef.current?.scrollToBottom(); };

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
