import type { Session } from '../lib/types';
import AgentGrid from './AgentGrid';

// CockpitTab is the full-size live grid (taller tiles than the Overview
// mini-grid). Clicking a pane pins + focuses that agent.
export default function CockpitTab({ sessions, onSelect }: {
  sessions: Session[];
  onSelect: (id: string) => void;
}) {
  return (
    <div className="cockpit">
      <AgentGrid sessions={sessions} onSelect={onSelect} lines={14} />
    </div>
  );
}
