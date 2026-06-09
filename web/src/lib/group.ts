import type { Session } from './types';

export interface SessionGroup {
  dir: string;
  sessions: Session[];
}

// UNKNOWN_DIR is the sentinel value used when neither repo nor workdir is set.
export const UNKNOWN_DIR = '—';

// sourceDir is the grouping key: the directory the warden command was
// triggered from. repo (typed/worktree agents) wins; otherwise workdir (prompt
// agents' caller cwd); UNKNOWN_DIR when neither is known.
export function sourceDir(s: Session): string {
  return s.repo || s.workdir || UNKNOWN_DIR;
}

// groupSessions buckets sessions by sourceDir and orders the groups by their
// most-recently-updated agent (desc). Within each group the input order is
// preserved (the daemon already returns updated_at-desc). Array sort is stable
// (ES2019+), so equal-recency groups keep first-seen order.
export function groupSessions(sessions: Session[]): SessionGroup[] {
  const groups = new Map<string, Session[]>();
  for (const s of sessions) {
    const k = sourceDir(s);
    const arr = groups.get(k);
    if (arr) arr.push(s);
    else groups.set(k, [s]);
  }
  const maxTs = (ss: Session[]) =>
    ss.reduce((m, s) => Math.max(m, new Date(s.updated_at).getTime() || 0), 0);
  return [...groups.entries()]
    .map(([dir, ss]) => ({ dir, sessions: ss }))
    .sort((a, b) => maxTs(b.sessions) - maxTs(a.sessions));
}

// baseName returns the last path segment of a grouping dir, for the pane title.
// A trailing slash is ignored. The '—' sentinel (unknown dir) and any input
// whose last segment is empty are returned unchanged.
export function baseName(dir: string): string {
  const trimmed = dir.replace(/\/+$/, '');
  const seg = trimmed.slice(trimmed.lastIndexOf('/') + 1);
  return seg || dir;
}
