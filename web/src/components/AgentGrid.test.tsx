import { act } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import AgentGrid from './AgentGrid';
import type { Session } from '../lib/types';
import type { ProjectTree, TreeNode } from '../lib/tree';
vi.mock('./MiniTerminal', () => ({ default: ({ id }: { id: string }) => <pre>{id} output</pre> }));
vi.mock('./QuickAddButton', () => ({ default: () => <button>Add agent</button> }));
const node = (type: TreeNode['type'], id: string, children: TreeNode[] = [], session_id?: string): TreeNode =>
  ({ type, id, label: id, status: 'idle', children, session_id });
const session = (id: string, extra: Partial<Session> = {}): Session => ({
  id, repo: '/misleading/path', workdir: '/misleading/path', status: 'working', ...extra,
} as Session);
let container: HTMLDivElement;
let root: Root;
beforeEach(() => {
  (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
  container = document.createElement('div'); document.body.append(container); root = createRoot(container);
});
afterEach(() => { act(() => root.unmount()); container.remove(); });
function render(tree: ProjectTree, sessions: Session[], collapsed = new Set<string>()) {
  const onSelect = vi.fn(), onTerminalSelect = vi.fn(), onToggleCollapse = vi.fn();
  act(() => root.render(<AgentGrid tree={tree} sessions={sessions} onSelect={onSelect}
    onTerminalSelect={onTerminalSelect} collapsed={collapsed} onToggleCollapse={onToggleCollapse} selectable />));
  return { onSelect, onTerminalSelect, onToggleCollapse };
}
function row(id: string) { return container.querySelector(`[data-node-id="${id}"]`)!; }
it('renders stored edges, jobs and flat project terminals regardless of paths and back-refs', () => {
  const tree = { roots: [node('project', 'stored-project', [
    node('agent', 'owner', [node('agent', 'child', [], 'child'),
      node('pipeline', 'pipeline', [node('job', 'job', [], 'job-agent')])], 'owner'),
    node('terminal', 'shell', [], 'shell'),
  ])] };
  const callbacks = render(tree, [session('child', { parent_id: 'wrong' }), session('owner'),
    session('job-agent'), session('shell', { kind: 'terminal' }), session('unlisted')]);
  expect(row('child').parentElement?.parentElement).toBe(row('owner'));
  expect(row('pipeline').parentElement?.parentElement).toBe(row('owner'));
  expect(row('job').parentElement?.parentElement).toBe(row('pipeline'));
  expect(row('shell').parentElement?.parentElement).toBe(row('stored-project'));
  expect(container.textContent).not.toContain('unlisted');
  expect(container.textContent).not.toContain('/misleading/path');
  expect(row('shell').querySelector('.tile-head')).toBeNull();
  expect(container.querySelectorAll('input[type="checkbox"]')).toHaveLength(3);
  act(() => (row('child').querySelector('.grid-tile') as HTMLButtonElement).click());
  expect(callbacks.onSelect).toHaveBeenCalledWith('child');
  act(() => (row('shell').querySelector('.grid-tile') as HTMLButtonElement).click());
  expect(callbacks.onTerminalSelect).toHaveBeenCalledWith('shell');
});
it('shows empty projects with zero sessions and never fills them from matching paths', () => {
  const tree = { roots: [node('project', 'empty')] };
  render(tree, []);
  expect(row('empty').textContent).toContain('No agents, pipelines, or terminals');
  render(tree, [session('unlisted')]);
  expect(row('empty').textContent).toContain('No agents, pipelines, or terminals');
  expect(container.querySelector('.grid-tile')).toBeNull();
});
it('retains unavailable sessions and queued jobs, indicates partial data, and collapses descendants', () => {
  const tree = { roots: [node('project', 'project', [node('pipeline', 'pipeline', [node('job', 'queued')]), node('agent', 'missing', [], 'missing')])], degraded: true, truncated: true };
  render(tree, []);
  expect(row('queued')).toBeTruthy(); expect(row('missing')).toBeTruthy();
  expect(container.textContent).toContain('unavailable'); expect(container.textContent).toContain('truncated');
  const callbacks = render(tree, [], new Set(['pipeline']));
  expect(container.querySelector('[data-node-id="queued"]')).toBeNull();
  act(() => (row('pipeline').querySelector('button') as HTMLButtonElement).click());
  expect(callbacks.onToggleCollapse).toHaveBeenCalledWith('pipeline');
});
it('preserves seven-state badges for agent and job tiles', () => {
  render({ roots: [node('project', 'project', [node('agent', 'a', [], 'a'), node('job', 'j', [], 'j')])] },
    [session('a', { status: 'waiting_for_input' }), session('j', { status: 'errored', exit_code: 1 })]);
  expect(row('a').querySelector('.badge')?.textContent).toBe('need-input');
  expect(row('j').querySelector('.badge')?.textContent).toBe('done');
});
