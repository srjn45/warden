// Wire contract shared by GET /tree and the named `tree` SSE event. Placement
// and ordering belong to the daemon; clients never infer them from paths.
export interface TreeNode {
  type: 'project' | 'agent' | 'terminal' | 'pipeline' | 'job' | 'autopilot_run' | 'manager' | 'guardian' | 'task' | 'worker';
  id: string;
  label: string;
  status: string;
  session_id?: string;
  detail?: {
    kind?: string;
    backend?: string;
    depends_on?: string[];
    repo?: string;
    path?: string;
    slot?: string;
    gate?: string;
    synthetic?: boolean;
    degraded?: boolean;
    closed?: boolean;
  };
  children?: TreeNode[] | null;
}

export interface ProjectTree {
  roots: TreeNode[] | null;
  degraded?: boolean;
  truncated?: boolean;
}

// Match the rendered preorder, including pipeline job sessions, while excluding
// terminal leaves, unavailable sessions, and descendants of collapsed nodes.
export function visibleAgentIds(nodes: TreeNode[], available: Set<string>, collapsed: Set<string>): string[] {
  const ids = new Set<string>();
  function visit(node: TreeNode) {
    if (node.type !== 'terminal' && node.session_id && available.has(node.session_id)) ids.add(node.session_id);
    if (!collapsed.has(node.id)) node.children?.forEach(visit);
  }
  nodes.forEach(visit);
  return [...ids];
}
