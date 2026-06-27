import { attachURL } from '../lib/attach';
import PtyTerminal from './PtyTerminal';

// AttachTerminal is the per-agent interactive terminal: a thin wrapper that
// points the shared PtyTerminal engine at one agent's attach WebSocket. Changing
// `id` reconnects to that agent. See PtyTerminal for the terminal mechanics.
export default function AttachTerminal({ id }: { id: string }) {
  return (
    <PtyTerminal
      reconnectKey={id}
      makeUrl={(loc) => attachURL(loc, id)}
    />
  );
}
