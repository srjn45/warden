import type { Session } from '../lib/types';
import AgentGrid from './AgentGrid';

// CockpitTab is the full-size live grid (taller tiles than the Overview
// mini-grid). Clicking a pane pins + focuses that agent; the per-pane '+'
// (wired via onCreated) spawns a new agent in that pane's directory.
export default function CockpitTab({ sessions, onSelect, onCreated }: {
  sessions: Session[];
  onSelect: (id: string) => void;
  onCreated: (id: string) => void;
}) {
  return (
    <div className="cockpit">
      <AgentGrid sessions={sessions} onSelect={onSelect} lines={14} onCreated={onCreated} />
    </div>
  );
}
