import { describe, it, expect, vi, beforeEach } from 'vitest';
import { runBatch, bulkTerminate, summarize } from './batch';
import * as api from './api';

beforeEach(() => { vi.restoreAllMocks(); });

describe('runBatch', () => {
  it('applies op to every id in order', async () => {
    const seen: string[] = [];
    const results = await runBatch(['a', 'b', 'c'], async (id) => { seen.push(id); });
    expect(seen).toEqual(['a', 'b', 'c']);
    expect(results.every((r) => r.ok)).toBe(true);
  });

  it('keeps going after a failure and records the error', async () => {
    const results = await runBatch(['a', 'b', 'c'], async (id) => {
      if (id === 'b') throw new Error('boom');
    });
    expect(results.map((r) => r.ok)).toEqual([true, false, true]);
    expect(results[1].error).toBe('boom');
  });
});

describe('bulkTerminate', () => {
  it('calls the terminate endpoint for each id', async () => {
    const spy = vi.spyOn(api, 'terminate').mockResolvedValue(undefined);
    const results = await bulkTerminate(['x', 'y']);
    expect(spy).toHaveBeenCalledTimes(2);
    expect(spy).toHaveBeenNthCalledWith(1, 'x');
    expect(spy).toHaveBeenNthCalledWith(2, 'y');
    expect(results.every((r) => r.ok)).toBe(true);
  });
});

describe('summarize', () => {
  it('reports all-success', () => {
    expect(summarize([{ id: 'a', ok: true }, { id: 'b', ok: true }])).toBe('2 succeeded');
  });
  it('reports partial failure', () => {
    expect(summarize([{ id: 'a', ok: true }, { id: 'b', ok: false, error: 'x' }]))
      .toBe('1 succeeded, 1 failed');
  });
});
