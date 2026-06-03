import { useEffect, useRef, useState } from 'react';
import { Terminal as XTerm } from '@xterm/xterm';
import { FitAddon } from '@xterm/addon-fit';
import '@xterm/xterm/css/xterm.css';
import { subscribeOutput } from '../lib/api';

// Terminal renders an agent's live tmux pane in color. tmux capture-pane returns
// the current visible screen (not a growing log), so each frame is a full
// snapshot: we reset() then write() the new frame.
export default function Terminal({ id }: { id: string }) {
  const hostRef = useRef<HTMLDivElement>(null);
  const [disconnected, setDisconnected] = useState(false);

  useEffect(() => {
    const host = hostRef.current;
    if (!host) return;
    setDisconnected(false);

    const term = new XTerm({
      convertEol: true,
      fontSize: 12,
      fontFamily: 'ui-monospace, SFMono-Regular, Menlo, monospace',
      theme: { background: '#0b0b0b', foreground: '#d6d6d6' },
      scrollback: 1000,
    });
    const fit = new FitAddon();
    term.loadAddon(fit);
    term.open(host);
    fit.fit();

    // Re-fit when the host container changes size (panel/layout shifts), not just
    // on window resize.
    const ro = new ResizeObserver(() => { try { fit.fit(); } catch { /* host detached */ } });
    ro.observe(host);

    const unsub = subscribeOutput(
      id,
      (frame) => { setDisconnected(false); term.reset(); term.write(frame); },
      () => setDisconnected(true), // EventSource auto-reconnects; next frame clears this
    );

    return () => {
      unsub();
      ro.disconnect();
      term.dispose();
    };
  }, [id]);

  return (
    <div className="xterm-wrap">
      {disconnected && (
        <div style={{ color: '#cf222e', fontSize: '.75rem', padding: '.15rem .4rem' }}>
          stream disconnected — retrying…
        </div>
      )}
      <div className="xterm-host" ref={hostRef} />
    </div>
  );
}
