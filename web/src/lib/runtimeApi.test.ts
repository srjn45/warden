import { describe, it, expect, vi, beforeEach } from 'vitest';
import { getSessionRuntime, getSessionDecisions } from './api';

function jsonResponse(body: unknown): Response {
  return new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } });
}

beforeEach(() => { vi.restoreAllMocks(); });

describe('runtime api', () => {
  it('getSessionRuntime GETs /sessions/{id}/runtime', async () => {
    const f = vi.fn().mockResolvedValue(jsonResponse({ session_id: 'A-1', mode: 'on', danger: true, posture: 'dangerously-execute-all' }));
    vi.stubGlobal('fetch', f);
    const v = await getSessionRuntime('A-1');
    expect(f.mock.calls[0][0]).toBe('/api/v1/sessions/A-1/runtime');
    expect(v.danger).toBe(true);
  });

  it('getSessionDecisions unwraps records and tolerates null', async () => {
    const f = vi.fn().mockResolvedValue(jsonResponse({ records: null }));
    vi.stubGlobal('fetch', f);
    expect(await getSessionDecisions('A-1', 5)).toEqual([]);
    expect(f.mock.calls[0][0]).toBe('/api/v1/sessions/A-1/decisions?limit=5');
  });
});
