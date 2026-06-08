// Pure layered-DAG layout for pipeline visualization. Computes a top-to-bottom
// layout from the depends_on edges alone (no backend change): each job sits one
// row below its deepest dependency (longest-path layering), so a chain reads
// top→bottom and independent jobs share a row side-by-side. Edges are directed
// dependency → dependent (the arrow points at the job that waits on it).

// Node box + spacing constants (px). Exported so the renderer and the edge math
// agree on geometry.
export const NODE_W = 168;
export const NODE_H = 72;
export const H_GAP = 28; // horizontal gap between sibling nodes in a row
export const V_GAP = 52; // vertical gap between dependency levels
export const PAD = 16; // canvas padding around the graph

export interface DagJob {
  id: string;
  depends_on?: string[] | null;
}

export interface DagNode<J extends DagJob> {
  id: string;
  job: J;
  level: number;
  x: number;
  y: number;
}

export interface DagEdge {
  from: string;
  to: string;
  x1: number;
  y1: number;
  x2: number;
  y2: number;
}

export interface DagLayout<J extends DagJob> {
  nodes: DagNode<J>[];
  edges: DagEdge[];
  width: number;
  height: number;
}

// assignLevels computes each job's row via longest-path layering: a job with no
// (known) dependencies is level 0; otherwise it is 1 + the max level of its
// dependencies. Unknown deps are ignored; cycles cannot loop forever because
// nodes currently being visited are pinned to level 0 (the daemon forbids cycles
// anyway — this is purely defensive).
export function assignLevels<J extends DagJob>(jobs: J[]): Record<string, number> {
  const byId = new Map<string, J>(jobs.map((j) => [j.id, j]));
  const level: Record<string, number> = {};
  const visiting = new Set<string>();

  function compute(id: string): number {
    if (id in level) return level[id];
    if (visiting.has(id)) return 0; // cycle guard: break the back-edge at 0
    const job = byId.get(id);
    if (!job) return 0;
    visiting.add(id);
    let max = -1;
    for (const dep of job.depends_on ?? []) {
      if (!byId.has(dep)) continue; // unknown dependency: treat as absent
      max = Math.max(max, compute(dep));
    }
    visiting.delete(id);
    level[id] = max + 1;
    return level[id];
  }

  for (const j of jobs) compute(j.id);
  return level;
}

// layoutDag positions every job on a grid and computes edge endpoints. Jobs keep
// their original order within a row, and each row is horizontally centered within
// the widest row so the graph reads as a balanced tree.
export function layoutDag<J extends DagJob>(jobs: J[]): DagLayout<J> {
  if (jobs.length === 0) return { nodes: [], edges: [], width: 0, height: 0 };

  const level = assignLevels(jobs);

  // Bucket jobs by level, preserving input order within each level.
  const rows = new Map<number, J[]>();
  let maxLevel = 0;
  for (const j of jobs) {
    const lv = level[j.id];
    maxLevel = Math.max(maxLevel, lv);
    const row = rows.get(lv) ?? [];
    row.push(j);
    rows.set(lv, row);
  }

  const rowWidth = (n: number) => n * NODE_W + (n - 1) * H_GAP;
  let widest = 0;
  for (const row of rows.values()) widest = Math.max(widest, rowWidth(row.length));

  const nodes: DagNode<J>[] = [];
  const pos = new Map<string, DagNode<J>>();
  for (let lv = 0; lv <= maxLevel; lv++) {
    const row = rows.get(lv) ?? [];
    const rowStart = PAD + (widest - rowWidth(row.length)) / 2;
    const y = PAD + lv * (NODE_H + V_GAP);
    row.forEach((job, i) => {
      const x = rowStart + i * (NODE_W + H_GAP);
      const node: DagNode<J> = { id: job.id, job, level: lv, x, y };
      nodes.push(node);
      pos.set(job.id, node);
    });
  }

  const edges: DagEdge[] = [];
  for (const j of jobs) {
    const to = pos.get(j.id)!;
    for (const dep of j.depends_on ?? []) {
      const from = pos.get(dep);
      if (!from) continue; // unknown dependency: no edge
      edges.push({
        from: dep,
        to: j.id,
        x1: from.x + NODE_W / 2,
        y1: from.y + NODE_H,
        x2: to.x + NODE_W / 2,
        y2: to.y,
      });
    }
  }

  const width = widest + 2 * PAD;
  const height = PAD + (maxLevel + 1) * (NODE_H + V_GAP) - V_GAP + PAD;
  return { nodes, edges, width, height };
}
