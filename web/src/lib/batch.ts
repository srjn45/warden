import { terminate, deleteSession, sendMessage } from './api';

// BatchResult records the outcome of one agent in a bulk operation. error is
// undefined on success; the human-readable message otherwise. The fan-out never
// throws — a failure on one agent must not abort the rest — so the caller
// inspects results to report partial success.
export interface BatchResult {
  id: string;
  ok: boolean;
  error?: string;
}

// runBatch applies op to each id in order (sequential fan-out — the
// goroutine-parallel backing is parked, #36), collecting a per-id result instead
// of failing fast. Exported for the typed wrappers below and unit-testable with
// an injected op.
export async function runBatch(
  ids: string[],
  op: (id: string) => Promise<void>,
): Promise<BatchResult[]> {
  const results: BatchResult[] = [];
  for (const id of ids) {
    try {
      await op(id);
      results.push({ id, ok: true });
    } catch (e) {
      results.push({ id, ok: false, error: e instanceof Error ? e.message : String(e) });
    }
  }
  return results;
}

export function bulkTerminate(ids: string[]): Promise<BatchResult[]> {
  return runBatch(ids, (id) => terminate(id));
}

export function bulkDelete(ids: string[], hard = false): Promise<BatchResult[]> {
  return runBatch(ids, (id) => deleteSession(id, hard));
}

export function bulkMessage(ids: string[], body: string): Promise<BatchResult[]> {
  return runBatch(ids, (id) => sendMessage(id, body));
}

// summarize folds batch results into "N succeeded, M failed" for a status line.
export function summarize(results: BatchResult[]): string {
  const ok = results.filter((r) => r.ok).length;
  const failed = results.length - ok;
  if (failed === 0) return `${ok} succeeded`;
  return `${ok} succeeded, ${failed} failed`;
}
