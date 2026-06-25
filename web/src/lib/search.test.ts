import { describe, it, expect } from 'vitest';
import { filterSessions } from './search';
import type { Session } from './types';

function sess(p: Partial<Session>): Session {
  return {
    id: '', type: '', ticket: '', tmux_session: '', repo: '', worktree: '',
    branch: '', pr: '', prompt: '', workdir: '', subject: '', status: 'working',
    pid: 0, created_at: '', updated_at: '', events: null, last_pane_excerpt: '',
    supervised: false, ...p,
  };
}

describe('filterSessions', () => {
  const sessions = [
    sess({ id: 'a', subject: 'fix the auth bug' }),
    sess({ id: 'b', prompt: 'refactor the payment flow' }),
    sess({ id: 'c', name: 'authster', type: 'pr-review' }),
    sess({ id: 'd', last_pane_excerpt: 'running go test ./auth/...' }),
  ];

  it('matches across subject, name, and pane excerpt (case-insensitive)', () => {
    const got = filterSessions(sessions, 'AUTH').map((s) => s.id);
    expect(got).toEqual(['a', 'c', 'd']);
  });

  it('matches prompt text', () => {
    expect(filterSessions(sessions, 'payment').map((s) => s.id)).toEqual(['b']);
  });

  it('ANDs multiple terms', () => {
    expect(filterSessions(sessions, 'auth pr-review').map((s) => s.id)).toEqual(['c']);
  });

  it('returns the full list for a blank query', () => {
    expect(filterSessions(sessions, '   ')).toHaveLength(4);
  });

  it('returns nothing when no session matches', () => {
    expect(filterSessions(sessions, 'nonsense')).toEqual([]);
  });
});
