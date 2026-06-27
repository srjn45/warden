import type { Session } from './types';

export interface SessionGroup {
  // key is the stable grouping identity (e.g. the dir path, type, status, or
  // tag). dir is the directory key, present only when grouping by dir (used by
  // QuickAddButton). label/sub are the display strings for the group header.
  key: string;
  label: string;
  sub?: string;
  dir?: string;
  sessions: Session[];
}

// GroupBy is the dimension AgentGrid buckets agents on. 'dir' is the historical
// default (directory the command ran from). 'type' / 'status' / 'tag' were added
// for #20 (agent grouping/filtering). 'backend' (#52) buckets by the AI agent
// driving each session and is labelled "Agent" in the UI.
export type GroupBy = 'dir' | 'type' | 'status' | 'tag' | 'backend';

export const GROUP_BY_VALUES: GroupBy[] = ['dir', 'type', 'status', 'tag', 'backend'];

export const GROUP_BY_LABELS: Record<GroupBy, string> = {
  dir: 'Directory',
  type: 'Type',
  status: 'Status',
  tag: 'Tag',
  backend: 'Agent',
};

// DEFAULT_BACKEND is what an agent with no recorded backend (omitempty JSON for
// pre-#52 Claude agents) groups and renders under.
export const DEFAULT_BACKEND = 'claude';

// UNKNOWN_DIR is the sentinel value used when neither repo nor workdir is set.
export const UNKNOWN_DIR = '—';

// Sentinel group keys for agents missing the grouping attribute.
export const UNTYPED = '(untyped)';
export const UNTAGGED = '(untagged)';

// sourceDir is the grouping key: the directory the warden command was
// triggered from. repo (typed/worktree agents) wins; otherwise workdir (prompt
// agents' caller cwd); UNKNOWN_DIR when neither is known.
export function sourceDir(s: Session): string {
  return s.repo || s.workdir || UNKNOWN_DIR;
}

// keysFor returns the grouping key(s) an agent belongs to for a given mode.
// Every mode yields exactly one key except 'tag': a multi-tagged agent appears
// in each of its tag groups, and an untagged agent falls into UNTAGGED.
function keysFor(s: Session, by: GroupBy): string[] {
  switch (by) {
    case 'type': return [s.type || UNTYPED];
    case 'backend': return [s.backend || DEFAULT_BACKEND];
    case 'status': return [s.status];
    case 'tag': {
      const tags = (s.tags ?? []).filter((t) => t.trim() !== '');
      return tags.length ? tags : [UNTAGGED];
    }
    case 'dir':
    default: return [sourceDir(s)];
  }
}

// groupSessionsBy buckets sessions on the chosen dimension and orders the groups
// by their newest agent's created_at (desc); within each group agents are ordered
// by created_at (desc). Ordering keys on the immutable created_at rather than
// updated_at so an agent's row is fixed at creation and only moves when agents are
// created/removed — keying on updated_at made the grid churn on every action
// (updated_at bumps whenever an agent does anything). Array sort is stable
// (ES2019+), so equal-created_at groups/agents keep first-seen order. For 'tag', a
// session may land in several groups; for every other mode each session lands in one.
export function groupSessionsBy(sessions: Session[], by: GroupBy): SessionGroup[] {
  const groups = new Map<string, Session[]>();
  for (const s of sessions) {
    for (const k of keysFor(s, by)) {
      const arr = groups.get(k);
      if (arr) arr.push(s);
      else groups.set(k, [s]);
    }
  }
  const createdMs = (s: Session) => new Date(s.created_at).getTime() || 0;
  const maxCreated = (ss: Session[]) => ss.reduce((m, s) => Math.max(m, createdMs(s)), 0);
  return [...groups.entries()]
    .map(([key, ss]) => decorate(key, [...ss].sort((a, b) => createdMs(b) - createdMs(a)), by))
    .sort((a, b) => maxCreated(b.sessions) - maxCreated(a.sessions));
}

// decorate attaches the display label/sub (and dir, for QuickAddButton) a group
// header needs, derived from its key and mode.
function decorate(key: string, sessions: Session[], by: GroupBy): SessionGroup {
  if (by === 'dir') {
    return { key, label: baseName(key), sub: key, dir: key, sessions };
  }
  if (by === 'status') {
    return { key, label: key.replace(/_/g, ' '), sessions };
  }
  return { key, label: key, sessions };
}

// groupSessions buckets sessions by sourceDir (the historical default view).
export function groupSessions(sessions: Session[]): SessionGroup[] {
  return groupSessionsBy(sessions, 'dir');
}

// baseName returns the last path segment of a grouping dir, for the pane title.
// A trailing slash is ignored. The '—' sentinel (unknown dir) and any input
// whose last segment is empty are returned unchanged.
export function baseName(dir: string): string {
  const trimmed = dir.replace(/\/+$/, '');
  const seg = trimmed.slice(trimmed.lastIndexOf('/') + 1);
  return seg || dir;
}
