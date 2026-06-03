import { useEffect, useRef, useState } from 'react';
import { Terminal as XTerm } from '@xterm/xterm';
import { FitAddon } from '@xterm/addon-fit';
import '@xterm/xterm/css/xterm.css';
import { attachURL, resizeMessage } from '../lib/attach';

// AttachTerminal is a fully interactive terminal bridged to a real `tmux attach`
// over a WebSocket: keystrokes (binary frames) go to the agent, PTY output
// (binary frames) is rendered, and fit/resize is sent as a text control frame.
export default function AttachTerminal({ id }: { id: string }) {
  const hostRef = useRef<HTMLDivElement>(null);
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

    const ws = new WebSocket(attachURL(window.location, id));
    ws.binaryType = 'arraybuffer';

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

    const dataSub = term.onData((d) => {
      if (ws.readyState === WebSocket.OPEN) ws.send(new TextEncoder().encode(d));
    });

    const ro = new ResizeObserver(() => sendResize());
    ro.observe(host);

    const onClick = () => term.focus();
    host.addEventListener('click', onClick);

    return () => {
      host.removeEventListener('click', onClick);
      ro.disconnect();
      dataSub.dispose();
      ws.close();
      term.dispose();
    };
  }, [id]);

  return (
    <div className="xterm-wrap">
      {closed && (
        <div style={{ color: '#cf222e', fontSize: '.75rem', padding: '.15rem .4rem' }}>
          disconnected — the agent may have ended
        </div>
      )}
      <div className="xterm-host" ref={hostRef} tabIndex={0} />
    </div>
  );
}
