import type { PipelineJob } from '../lib/types';
import { layoutDag, NODE_W, NODE_H } from '../lib/dag';
import { jobStatusClass } from '../lib/pipelines';

// PipelineDag renders a pipeline as a layered directed graph: jobs are laid out
// top-to-bottom by dependency depth (independent jobs share a row, side-by-side),
// with arrows drawn dependency → dependent. Layout geometry comes entirely from
// the pure lib/dag module (no backend change); this component only paints it.
// Clicking a node selects the job (the detail drawer carries retry / open-terminal).
export default function PipelineDag({ jobs, selected, onSelect }: {
  jobs: PipelineJob[];
  selected: string | null;
  onSelect: (id: string) => void;
}) {
  const { nodes, edges, width, height } = layoutDag(jobs);

  return (
    <div className="dag" style={{ width, height }} role="group" aria-label="pipeline graph">
      <svg className="dag-edges" width={width} height={height} aria-hidden="true">
        <defs>
          <marker id="dag-arrow" viewBox="0 0 10 10" refX="9" refY="5"
            markerWidth="7" markerHeight="7" orient="auto-start-reverse">
            <path d="M0,0 L10,5 L0,10 z" fill="var(--border-strong)" />
          </marker>
        </defs>
        {edges.map((e) => (
          <line
            key={`${e.from}->${e.to}`}
            x1={e.x1} y1={e.y1} x2={e.x2} y2={e.y2}
            stroke="var(--border-strong)" strokeWidth={1.5} markerEnd="url(#dag-arrow)"
          />
        ))}
      </svg>
      {nodes.map((n) => {
        const j = n.job;
        return (
          <button
            key={n.id}
            className={`dag-node ${jobStatusClass(j.status)}${n.id === selected ? ' on' : ''}`}
            style={{ left: n.x, top: n.y, width: NODE_W, height: NODE_H }}
            onClick={() => onSelect(n.id)}
            title={j.prompt}
          >
            <span className="dag-node-id">{j.id}</span>
            <span className="dag-node-st">{j.status}</span>
          </button>
        );
      })}
    </div>
  );
}
