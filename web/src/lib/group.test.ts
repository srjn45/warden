import { describe, it, expect } from 'vitest';
import { groupSessions, sourceDir } from './group';
import type { Session } from './types';

function sess(p: Partial<Session>): Session {
  return {
    id: '', type: '', ticket: '', tmux_session: '', repo: '', worktree: '',
    branch: '', pr: '', prompt: '', workdir: '', subject: '', status: 'idle',
    pid: 0, created_at: '', updated_at: '', events: null, last_pane_excerpt: '', supervised: false,
    ...p,
  };
}

describe('sourceDir', () => {
  it('prefers repo, then workdir, then dash', () => {
    expect(sourceDir(sess({ repo: '/r', workdir: '/r/.worktrees/x' }))).toBe('/r');
    expect(sourceDir(sess({ workdir: '/w' }))).toBe('/w');
    expect(sourceDir(sess({}))).toBe('—');
  });
});

describe('groupSessions', () => {
  it('groups by source dir, orders groups by recency, preserves within-group order', () => {
    const out = groupSessions([
      sess({ id: 'b1', workdir: '/b', updated_at: '2026-06-03T10:00:00Z' }),
      sess({ id: 'a1', workdir: '/a', updated_at: '2026-06-03T09:00:00Z' }),
      sess({ id: 'b2', workdir: '/b', updated_at: '2026-06-03T08:00:00Z' }),
      sess({ id: 'a2', workdir: '/a', updated_at: '2026-06-03T07:00:00Z' }),
    ]);
    expect(out.map((g) => g.dir)).toEqual(['/b', '/a']);
    expect(out[0].sessions.map((s) => s.id)).toEqual(['b1', 'b2']);
    expect(out[1].sessions.map((s) => s.id)).toEqual(['a1', 'a2']);
  });

  it('returns one group when all share a dir', () => {
    const out = groupSessions([
      sess({ id: 'x', workdir: '/w', updated_at: '2026-06-03T10:00:00Z' }),
      sess({ id: 'y', workdir: '/w', updated_at: '2026-06-03T09:00:00Z' }),
    ]);
    expect(out).toHaveLength(1);
    expect(out[0].dir).toBe('/w');
    expect(out[0].sessions).toHaveLength(2);
  });
});
