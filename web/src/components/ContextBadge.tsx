import { fmtTokens, contextClass, known } from '../lib/context';

// ContextBadge shows an agent's context-window fill, tinted green/orange/red.
// Renders nothing when the gauge is unknown (just-spawned, no model turn yet).
export default function ContextBadge({ tokens, state }: { tokens?: number; state?: string }) {
  if (!known(tokens, state)) return null;
  return (
    <span className={`ctx-badge ${contextClass(state)}`} title={`context ~${fmtTokens(tokens)} (${state})`}>
      {fmtTokens(tokens)}
    </span>
  );
}
