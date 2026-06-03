import { describe, it, expect } from 'vitest';
import { splitPath, filterEntries } from './dirpath';
import type { DirEntry } from './api';

describe('splitPath', () => {
  it('trailing slash → baseDir is the dir, empty leaf', () => {
    expect(splitPath('/a/b/')).toEqual({ baseDir: '/a/b', leaf: '' });
  });
  it('no trailing slash → dirname + leaf', () => {
    expect(splitPath('/a/b')).toEqual({ baseDir: '/a', leaf: 'b' });
  });
  it('root variants', () => {
    expect(splitPath('/')).toEqual({ baseDir: '/', leaf: '' });
    expect(splitPath('/x')).toEqual({ baseDir: '/', leaf: 'x' });
  });
  it('no slash → empty baseDir (backend home), whole string is the leaf', () => {
    expect(splitPath('foo')).toEqual({ baseDir: '', leaf: 'foo' });
  });
  it('empty → both empty', () => {
    expect(splitPath('')).toEqual({ baseDir: '', leaf: '' });
  });
});

describe('filterEntries', () => {
  const entries: DirEntry[] = [
    { name: 'workspace', path: '/u/workspace' },
    { name: 'Work-notes', path: '/u/Work-notes' },
    { name: 'Documents', path: '/u/Documents' },
  ];
  it('empty leaf returns all entries', () => {
    expect(filterEntries(entries, '')).toHaveLength(3);
  });
  it('prefix match, case-insensitive', () => {
    expect(filterEntries(entries, 'wor').map((e) => e.name)).toEqual(['workspace', 'Work-notes']);
  });
  it('no match returns empty', () => {
    expect(filterEntries(entries, 'zzz')).toEqual([]);
  });
});
