import type { ContextEntry } from './types';

// The inspector is the web's read-only window onto the daemon's shared state:
// the namespaced context KV store and recent inter-agent message traffic. These
// helpers are pure (no fetch/DOM) so the grouping logic is unit-testable.

// contextNamespace returns the leading dot-segment of a context key — the
// namespace agents group writes under (e.g. "pipeline" for
// "pipeline.x.job.output"), or "(root)" when the key has no dot.
export function contextNamespace(key: string): string {
  const i = key.indexOf('.');
  return i === -1 ? '(root)' : key.slice(0, i);
}

export interface ContextGroup {
  namespace: string;
  entries: ContextEntry[];
}

// groupContext buckets entries by namespace, sorts keys within each group, and
// returns the groups ordered by namespace name. Pure: inputs are not mutated.
export function groupContext(entries: ContextEntry[]): ContextGroup[] {
  const byNs = new Map<string, ContextEntry[]>();
  for (const e of entries) {
    const ns = contextNamespace(e.key);
    const arr = byNs.get(ns) ?? [];
    arr.push(e);
    byNs.set(ns, arr);
  }
  return [...byNs.entries()]
    .map(([namespace, es]) => ({
      namespace,
      entries: [...es].sort((a, b) => a.key.localeCompare(b.key)),
    }))
    .sort((a, b) => a.namespace.localeCompare(b.namespace));
}
