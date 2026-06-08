import { describe, it, expect } from 'vitest';
import { assignLevels, layoutDag, NODE_W, NODE_H } from './dag';

type J = { id: string; depends_on?: string[] | null };

function jobs(spec: Record<string, string[]>): J[] {
  return Object.entries(spec).map(([id, deps]) => ({ id, depends_on: deps }));
}

describe('assignLevels', () => {
  it('puts dependency-free jobs at level 0', () => {
    const lv = assignLevels(jobs({ a: [], b: [] }));
    expect(lv).toEqual({ a: 0, b: 0 });
  });

  it('uses longest-path layering for a linear chain', () => {
    const lv = assignLevels(jobs({ a: [], b: ['a'], c: ['b'] }));
    expect(lv).toEqual({ a: 0, b: 1, c: 2 });
  });

  it('places a job one below its deepest dependency (diamond)', () => {
    // a -> b, a -> c, then d depends on both b and c.
    const lv = assignLevels(jobs({ a: [], b: ['a'], c: ['a'], d: ['b', 'c'] }));
    expect(lv).toEqual({ a: 0, b: 1, c: 1, d: 2 });
  });

  it('uses longest path when deps sit at different depths', () => {
    // d depends on a (level 0) and c (level 2) -> d must be level 3.
    const lv = assignLevels(jobs({ a: [], b: ['a'], c: ['b'], d: ['a', 'c'] }));
    expect(lv).toEqual({ a: 0, b: 1, c: 2, d: 3 });
  });

  it('tolerates an unknown dependency by treating it as absent', () => {
    const lv = assignLevels(jobs({ a: ['ghost'] }));
    expect(lv).toEqual({ a: 0 });
  });

  it('does not loop forever on a cycle', () => {
    // Defensive: the server forbids cycles, but the layout must still terminate.
    const lv = assignLevels(jobs({ a: ['b'], b: ['a'] }));
    expect(typeof lv.a).toBe('number');
    expect(typeof lv.b).toBe('number');
  });

  it('handles null depends_on', () => {
    const lv = assignLevels([{ id: 'a', depends_on: null }]);
    expect(lv).toEqual({ a: 0 });
  });
});

describe('layoutDag', () => {
  it('returns a node per job with its level', () => {
    const lay = layoutDag(jobs({ a: [], b: ['a'] }));
    expect(lay.nodes.map((n) => n.id).sort()).toEqual(['a', 'b']);
    const byId = Object.fromEntries(lay.nodes.map((n) => [n.id, n]));
    expect(byId.a.level).toBe(0);
    expect(byId.b.level).toBe(1);
  });

  it('stacks levels vertically (deeper level = larger y)', () => {
    const lay = layoutDag(jobs({ a: [], b: ['a'] }));
    const byId = Object.fromEntries(lay.nodes.map((n) => [n.id, n]));
    expect(byId.b.y).toBeGreaterThan(byId.a.y);
  });

  it('lays parallel jobs in the same level side-by-side (same y, different x)', () => {
    const lay = layoutDag(jobs({ a: [], b: [] }));
    const byId = Object.fromEntries(lay.nodes.map((n) => [n.id, n]));
    expect(byId.a.y).toBe(byId.b.y);
    expect(byId.a.x).not.toBe(byId.b.x);
  });

  it('keeps the original job order within a level', () => {
    const lay = layoutDag(jobs({ a: [], b: [], c: [] }));
    const lvl0 = lay.nodes.filter((n) => n.level === 0).sort((m, n) => m.x - n.x);
    expect(lvl0.map((n) => n.id)).toEqual(['a', 'b', 'c']);
  });

  it('emits one edge per dependency, directed dep -> dependent', () => {
    const lay = layoutDag(jobs({ a: [], b: ['a'], c: ['a', 'b'] }));
    const pairs = lay.edges.map((e) => `${e.from}->${e.to}`).sort();
    expect(pairs).toEqual(['a->b', 'a->c', 'b->c']);
  });

  it('skips edges for unknown dependency targets', () => {
    const lay = layoutDag(jobs({ a: ['ghost'] }));
    expect(lay.edges).toEqual([]);
  });

  it('draws edges from the bottom of the dep to the top of the dependent', () => {
    const lay = layoutDag(jobs({ a: [], b: ['a'] }));
    const byId = Object.fromEntries(lay.nodes.map((n) => [n.id, n]));
    const e = lay.edges.find((x) => x.from === 'a' && x.to === 'b')!;
    expect(e.x1).toBeCloseTo(byId.a.x + NODE_W / 2);
    expect(e.y1).toBeCloseTo(byId.a.y + NODE_H);
    expect(e.x2).toBeCloseTo(byId.b.x + NODE_W / 2);
    expect(e.y2).toBeCloseTo(byId.b.y);
  });

  it('reports a positive canvas size that contains every node', () => {
    const lay = layoutDag(jobs({ a: [], b: ['a'], c: ['a'] }));
    expect(lay.width).toBeGreaterThan(0);
    expect(lay.height).toBeGreaterThan(0);
    for (const n of lay.nodes) {
      expect(n.x + NODE_W).toBeLessThanOrEqual(lay.width);
      expect(n.y + NODE_H).toBeLessThanOrEqual(lay.height);
    }
  });

  it('handles an empty pipeline', () => {
    const lay = layoutDag([]);
    expect(lay.nodes).toEqual([]);
    expect(lay.edges).toEqual([]);
  });

  it('preserves the original job object on each node', () => {
    const list = [{ id: 'a', depends_on: [], status: 'done' }];
    const lay = layoutDag(list as any);
    expect((lay.nodes[0].job as any).status).toBe('done');
  });
});
