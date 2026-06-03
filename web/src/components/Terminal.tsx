import { useEffect, useRef } from 'react';
import { Terminal as XTerm } from '@xterm/xterm';
import { FitAddon } from '@xterm/addon-fit';
import '@xterm/xterm/css/xterm.css';
import { subscribeOutput } from '../lib/api';

// Terminal renders an agent's live tmux pane in color. tmux capture-pane returns
// the current visible screen (not a growing log), so each frame is a full
// snapshot: we reset() then write() the new frame.
export default function Terminal({ id }: { id: string }) {
  const hostRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const host = hostRef.current;
    if (!host) return;

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

    const onResize = () => { try { fit.fit(); } catch { /* host detached */ } };
    window.addEventListener('resize', onResize);

    const unsub = subscribeOutput(id, (frame) => {
      term.reset();
      term.write(frame);
    });

    return () => {
      unsub();
      window.removeEventListener('resize', onResize);
      term.dispose();
    };
  }, [id]);

  return <div className="xterm-host" ref={hostRef} />;
}
