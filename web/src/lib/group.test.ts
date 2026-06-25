import { describe, it, expect } from 'vitest';
import { groupSessions, groupSessionsBy, sourceDir, baseName, UNTYPED, UNTAGGED } from './group';
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

describe('groupSessionsBy', () => {
  it('groups by type and falls back to the untyped sentinel', () => {
    const out = groupSessionsBy([
      sess({ id: 'a', type: 'feature', updated_at: '2026-06-03T10:00:00Z' }),
      sess({ id: 'b', type: '', updated_at: '2026-06-03T09:00:00Z' }),
      sess({ id: 'c', type: 'feature', updated_at: '2026-06-03T08:00:00Z' }),
    ], 'type');
    expect(out.map((g) => g.key)).toEqual(['feature', UNTYPED]);
    expect(out[0].label).toBe('feature');
    expect(out[0].sessions.map((s) => s.id)).toEqual(['a', 'c']);
    expect(out[1].sessions.map((s) => s.id)).toEqual(['b']);
  });

  it('groups by status and humanizes the label', () => {
    const out = groupSessionsBy([
      sess({ id: 'a', status: 'working', updated_at: '2026-06-03T10:00:00Z' }),
      sess({ id: 'b', status: 'waiting_for_input', updated_at: '2026-06-03T11:00:00Z' }),
    ], 'status');
    expect(out.map((g) => g.key)).toEqual(['waiting_for_input', 'working']);
    expect(out[0].label).toBe('waiting for input');
  });

  it('groups by tag, places a multi-tagged agent in every group, and buckets untagged', () => {
    const out = groupSessionsBy([
      sess({ id: 'a', tags: ['backend', 'urgent'], updated_at: '2026-06-03T10:00:00Z' }),
      sess({ id: 'b', tags: ['backend'], updated_at: '2026-06-03T09:00:00Z' }),
      sess({ id: 'c', tags: [], updated_at: '2026-06-03T08:00:00Z' }),
      sess({ id: 'd', updated_at: '2026-06-03T07:00:00Z' }),
    ], 'tag');
    const byKey = Object.fromEntries(out.map((g) => [g.key, g.sessions.map((s) => s.id)]));
    expect(byKey['backend']).toEqual(['a', 'b']);
    expect(byKey['urgent']).toEqual(['a']);
    expect(byKey[UNTAGGED]).toEqual(['c', 'd']);
  });

  it('decorates dir groups with label/sub/dir for the quick-add button', () => {
    const out = groupSessionsBy([
      sess({ id: 'a', repo: '/Users/x/warden', updated_at: '2026-06-03T10:00:00Z' }),
    ], 'dir');
    expect(out[0]).toMatchObject({ key: '/Users/x/warden', label: 'warden', sub: '/Users/x/warden', dir: '/Users/x/warden' });
  });
});

describe('baseName', () => {
  it('returns the last path segment', () => {
    expect(baseName('/Users/x/workspace/personal/warden')).toBe('warden');
  });
  it('ignores a trailing slash', () => {
    expect(baseName('/Users/x/warden/')).toBe('warden');
  });
  it('returns the dash sentinel as-is', () => {
    expect(baseName('—')).toBe('—');
  });
  it('returns a bare name unchanged', () => {
    expect(baseName('warden')).toBe('warden');
  });
  it('falls back to the original when there is no segment', () => {
    expect(baseName('/')).toBe('/');
  });
});
