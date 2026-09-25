import type { Session } from '../lib/types';
import type { ProjectTree, TreeNode } from '../lib/tree';
import MiniTerminal from './MiniTerminal';
import BusyIdleBadge from './BusyIdleBadge';
import ContextBadge from './ContextBadge';
import BackendLogo from './BackendLogo';
import QuickAddButton from './QuickAddButton';

// Render daemon placement verbatim. Session snapshots enrich tiles, never decide
// their membership, parent, or ordering. Missing sessions remain visible as rows.
export default function AgentGrid({ tree, sessions, onSelect, onTerminalSelect, lines = 8, onCreated, selectable, selected, onToggleSelect, collapsed, onToggleCollapse }: {
  tree: ProjectTree;
  sessions: Session[];
  onSelect: (id: string) => void;
  onTerminalSelect: (id: string) => void;
  lines?: number;
  onCreated?: (id: string) => void;
  selectable?: boolean;
  selected?: Set<string>;
  onToggleSelect?: (id: string, shift: boolean) => void;
  collapsed: Set<string>;
  onToggleCollapse: (id: string) => void;
}) {
  const byId = new Map(sessions.map((s) => [s.id, s]));

  function renderNode(node: TreeNode) {
    const session = node.session_id ? byId.get(node.session_id) : undefined;
    const terminal = node.type === 'terminal';
    const children = node.children ?? [];
    const isCollapsed = collapsed.has(node.id);
    const isSelected = !!session && (selected?.has(session.id) ?? false);
    const project = node.type === 'project';
    const dir = node.detail?.path;
    return (
      <li key={node.id} data-node-id={node.id} data-node-type={node.type} style={{ listStyle: 'none', marginBlock: '0.5rem' }}>
        <div className="grid-group-bar">
          {children.length > 0 ? (
            <button className="grid-group-toggle" aria-expanded={!isCollapsed}
              aria-label={`${isCollapsed ? 'Expand' : 'Collapse'} ${node.label}`}
              onClick={() => onToggleCollapse(node.id)}>
              <span className="grid-group-caret">{isCollapsed ? '▸' : '▾'}</span>
              <span className="grid-group-name">{node.label}</span>
            </button>
          ) : <span className="grid-group-name">{node.label}</span>}
          <span className="muted">{node.type.replaceAll('_', ' ')}</span>
          {/* Container/job statuses use the tree vocabulary; agent tiles below
              use the existing seven-state badge with exit-code handling. */}
          {(!session || terminal) && <span className="muted">{node.status}</span>}
          {node.detail?.closed && <span className="muted">closed</span>}
          {node.detail?.degraded && <span className="warn">Partially available</span>}
          {project && onCreated && dir && !node.detail?.closed && !node.detail?.synthetic && (
            <QuickAddButton dir={dir} onCreated={onCreated} />
          )}
        </div>
        {session && (terminal ? (
          <button className="grid-tile" onClick={() => onTerminalSelect(session.id)}>
            Open terminal {node.label}
          </button>
        ) : (
          <div className="agent-grid">
            <div className={`grid-tile-wrap${isSelected ? ' selected' : ''}`}>
              {selectable && <input type="checkbox" className="tile-select" checked={isSelected}
                aria-label={`Select ${session.id}`} onChange={() => { /* controlled via click */ }}
                onClick={(e) => onToggleSelect?.(session.id, e.shiftKey)} />}
              <button className="grid-tile" onClick={() => onSelect(session.id)}>
                <div className="tile-head">
                  <BackendLogo backend={session.backend} />
                  <b>{session.id}</b> <BusyIdleBadge status={session.status} exitCode={session.exit_code} />
                  <ContextBadge tokens={session.context_tokens} state={session.context_state} />
                </div>
                <MiniTerminal id={session.id} lines={lines} />
              </button>
            </div>
          </div>
        ))}
        {project && children.length === 0 && (
          <p className="muted">No agents, pipelines, or terminals in this project yet.</p>
        )}
        {!isCollapsed && children.length > 0 && (
          <ul aria-label={`${node.label} members`} style={{ paddingInlineStart: '1.25rem', borderInlineStart: '1px solid var(--border)' }}>
            {children.map(renderNode)}
          </ul>
        )}
      </li>
    );
  }

  return (
    <>
      {tree.degraded && <p className="warn" role="status">Some project hierarchy data is unavailable.</p>}
      {tree.truncated && <p className="warn" role="status">Project hierarchy is truncated.</p>}
      {(tree.roots?.length ?? 0) === 0 ? <p className="muted">No projects yet.</p> : (
        <ul className="agent-grid-groups" aria-label="Project hierarchy" style={{ padding: 0 }}>
          {tree.roots?.map(renderNode)}
        </ul>
      )}
    </>
  );
}
