import type { DirEntry } from './api';

// splitPath divides a typed path into the directory whose children should be
// listed (baseDir) and the trailing segment used to filter them (leaf).
// A trailing slash means "I'm inside this directory" (no filter); otherwise the
// last segment filters the parent's children. An empty baseDir means the backend
// default (home).
export function splitPath(query: string): { baseDir: string; leaf: string } {
  if (query.endsWith('/')) {
    const trimmed = query.slice(0, -1);
    return { baseDir: trimmed === '' ? '/' : trimmed, leaf: '' };
  }
  const i = query.lastIndexOf('/');
  if (i < 0) return { baseDir: '', leaf: query };
  const baseDir = query.slice(0, i);
  return { baseDir: baseDir === '' ? '/' : baseDir, leaf: query.slice(i + 1) };
}

// filterEntries keeps the subdirectories whose name starts with leaf
// (case-insensitive). An empty leaf returns every entry.
export function filterEntries(entries: DirEntry[], leaf: string): DirEntry[] {
  if (leaf === '') return entries;
  const p = leaf.toLowerCase();
  return entries.filter((e) => e.name.toLowerCase().startsWith(p));
}
