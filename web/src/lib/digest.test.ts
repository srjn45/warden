import { describe, it, expect } from 'vitest';
import { fileLabel, hasFiles } from './digest';
import type { Digest } from './types';

const base: Digest = {
  summary: 's', files: [], branch: 'main', turns: 1, status: 'idle', task: 't',
};

describe('digest formatting', () => {
  it('formats a file with +/- and edit marker', () => {
    expect(fileLabel({ path: 'a.go', added: 3, removed: 1, edited: true }))
      .toBe('* a.go  +3 -1');
  });
  it('uses a space marker for non-edited (git-only) files', () => {
    expect(fileLabel({ path: 'b.go', added: 0, removed: 2, edited: false }))
      .toBe('  b.go  +0 -2');
  });
  it('hasFiles is false for null or empty', () => {
    expect(hasFiles({ ...base, files: null })).toBe(false);
    expect(hasFiles({ ...base, files: [] })).toBe(false);
    expect(hasFiles({ ...base, files: [{ path: 'x', added: 0, removed: 0, edited: true }] })).toBe(true);
  });
});
