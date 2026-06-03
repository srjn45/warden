import { useEffect, useRef, useState } from 'react';
import { getOutput } from '../lib/api';

// MiniTerminal is a cheap glance at an agent's pane for the grid tiles: it polls
// the plain (uncolored) output endpoint and shows the last `lines` rows. Not a
// live feed — that's what the full Terminal is for.
export default function MiniTerminal({ id, lines = 8, intervalMs = 2000 }: {
  id: string;
  lines?: number;
  intervalMs?: number;
}) {
  const [text, setText] = useState('');
  const preRef = useRef<HTMLPreElement>(null);

  useEffect(() => {
    let alive = true;
    const poll = async () => {
      try {
        const o = await getOutput(id, lines);
        if (alive) setText(o.split('\n').slice(-lines).join('\n'));
      } catch { /* agent may have ended; SSE list will drop it */ }
    };
    poll();
    const t = setInterval(poll, intervalMs);
    return () => { alive = false; clearInterval(t); };
  }, [id, lines, intervalMs]);

  useEffect(() => {
    if (preRef.current) preRef.current.scrollTop = preRef.current.scrollHeight;
  }, [text]);

  return <pre className="mini-term" ref={preRef}>{text || '…'}</pre>;
}
