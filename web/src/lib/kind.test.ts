import { describe, it, expect } from 'vitest';
import { isTerminal, partitionByKind, terminalName } from './kind';
import type { Session } from './types';

// A minimal Session factory — only the fields these helpers read matter.
function sess(over: Partial<Session>): Session {
  return {
    id: 'x', type: '', ticket: '', tmux_session: '', repo: '', worktree: '',
    branch: '', pr: '', prompt: '', workdir: '', subject: '', status: 'working',
    pid: 0, created_at: '', updated_at: '', events: null, last_pane_excerpt: '',
    supervised: false, ...over,
  };
}

describe('isTerminal', () => {
  it('is true only for an explicit terminal kind', () => {
    expect(isTerminal(sess({ kind: 'terminal' }))).toBe(true);
  });
  it('treats absent/empty/agent kind as an agent (back-compat)', () => {
    expect(isTerminal(sess({}))).toBe(false);
    expect(isTerminal(sess({ kind: '' }))).toBe(false);
    expect(isTerminal(sess({ kind: 'agent' }))).toBe(false);
  });
});

describe('partitionByKind', () => {
  it('splits agents from terminals, preserving order', () => {
    const list = [
      sess({ id: 'a1' }),
      sess({ id: 't1', kind: 'terminal' }),
      sess({ id: 'a2', kind: 'agent' }),
      sess({ id: 't2', kind: 'terminal' }),
    ];
    const { agents, terminals } = partitionByKind(list);
    expect(agents.map((s) => s.id)).toEqual(['a1', 'a2']);
    expect(terminals.map((s) => s.id)).toEqual(['t1', 't2']);
  });
  it('returns empty arrays for an empty list', () => {
    const { agents, terminals } = partitionByKind([]);
    expect(agents).toEqual([]);
    expect(terminals).toEqual([]);
  });
});

describe('terminalName', () => {
  it('prefers the explicit name', () => {
    expect(terminalName(sess({ name: 'scratch', workdir: '/home/x/warden' }))).toBe('scratch');
  });
  it('falls back to the workdir base name, with branch when known', () => {
    expect(terminalName(sess({ workdir: '/home/x/warden', kind: 'terminal' }))).toBe('warden');
    expect(terminalName(sess({ repo: '/home/x/warden', branch: 'main', kind: 'terminal' })))
      .toBe('warden (main)');
  });
  it('falls back to the id when no dir is known', () => {
    expect(terminalName(sess({ id: 't9', kind: 'terminal' }))).toBe('t9');
  });
});
