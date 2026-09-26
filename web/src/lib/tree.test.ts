import { expect, it } from 'vitest';
import { visibleAgentIds, type TreeNode } from './tree';
it('selects available agents and jobs in visible tree order, excluding terminals and collapsed descendants', () => {
  const nodes: TreeNode[] = [{ type: 'project', id: 'p', label: 'p', status: 'idle', children: [
    { type: 'agent', id: 'a', label: 'a', status: 'busy', session_id: 'a', children: [
      { type: 'agent', id: 'child', label: 'child', status: 'idle', session_id: 'child' },
      { type: 'pipeline', id: 'pipe', label: 'pipe', status: 'active', children: [
        { type: 'job', id: 'j', label: 'j', status: 'active', session_id: 'job-agent' },
        { type: 'job', id: 'missing', label: 'missing', status: 'done', session_id: 'missing' },
      ] },
    ] },
    { type: 'terminal', id: 't', label: 't', status: 'busy', session_id: 't' },
  ] }];
  const available = new Set(['t', 'job-agent', 'child', 'a', 'unlisted']);
  expect(visibleAgentIds(nodes, available, new Set())).toEqual(['a', 'child', 'job-agent']);
  expect(visibleAgentIds(nodes, available, new Set(['pipe']))).toEqual(['a', 'child']);
  expect(visibleAgentIds(nodes, available, new Set(['a']))).toEqual(['a']);
  expect(visibleAgentIds(nodes, available, new Set(['p']))).toEqual([]);
});
