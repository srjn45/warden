import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { FleetRefreshCoordinator, formatFleetBanner, type FleetRefreshState } from './refresh';
import { ApiError } from './api';
import type { Session } from './types';

function createMockSession(id: string): Session {
  return {
    id,
    name: id,
    role: 'worker',
    status: 'working',
    kind: 'agent',
    subject: '',
    prompt: '',
    cwd: '/work',
    workdir: '/work',
    repo: '/repo',
    branch: 'main',
    pr: null,
    ticket: null,
    agent_backend: null,
    events: null,
    created_at: 0,
    updated_at: 0,
    tags: null,
  };
}

describe('refresh orchestration', () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
    vi.restoreAllMocks();
  });

  it('formats banner for degraded state with last-complete time', () => {
    const at = new Date(2026, 8, 31, 14, 5, 1);
    const state: FleetRefreshState = {
      sessions: [],
      tree: null,
      treeError: null,
      status: 'degraded',
      lastCompleteAt: at,
      inFlight: false,
      failures: 1,
    };
    const banner = formatFleetBanner(state);
    expect(banner.show).toBe(true);
    expect(banner.severity).toBe('warning');
    expect(banner.text).toBe('session store degraded — run `warden doctor` · showing last complete fleet from 14:05:01');
  });

  it('formats banner for disconnected state with last-complete time', () => {
    const at = new Date(2026, 8, 31, 14, 5, 1);
    const state: FleetRefreshState = {
      sessions: [],
      tree: null,
      treeError: null,
      status: 'disconnected',
      lastCompleteAt: at,
      inFlight: false,
      failures: 1,
    };
    const banner = formatFleetBanner(state);
    expect(banner.show).toBe(true);
    expect(banner.severity).toBe('error');
    expect(banner.text).toBe('daemon not running — start it with `warden daemon` · showing last complete fleet from 14:05:01');
  });

  it('formats banner for disconnected state without prior snapshot', () => {
    const state: FleetRefreshState = {
      sessions: [],
      tree: null,
      treeError: null,
      status: 'disconnected',
      lastCompleteAt: null,
      inFlight: false,
      failures: 1,
    };
    const banner = formatFleetBanner(state);
    expect(banner.show).toBe(true);
    expect(banner.severity).toBe('error');
    expect(banner.text).toBe('daemon not running — start it with `warden daemon`');
  });

  it('hides banner when live', () => {
    const state: FleetRefreshState = {
      sessions: [],
      tree: null,
      treeError: null,
      status: 'live',
      lastCompleteAt: new Date(),
      inFlight: false,
      failures: 0,
    };
    expect(formatFleetBanner(state).show).toBe(false);
  });

  it('coalesces concurrent refresh requests into one in-flight fetch', async () => {
    let resolveFirst: (data: { sessions: Session[] }) => void;
    let fetchCalls = 0;

    const fetchFleet = vi.fn().mockImplementation(() => {
      fetchCalls++;
      if (fetchCalls === 1) {
        return new Promise((res) => { resolveFirst = res; });
      }
      return Promise.resolve({ sessions: [createMockSession('s2')] });
    });

    const coordinator = new FleetRefreshCoordinator({
      fetchFleet,
      subscribeSSE: () => () => {},
    });

    coordinator.refresh(); // call 1: starts in-flight
    expect(coordinator.getState().inFlight).toBe(true);
    expect(fetchCalls).toBe(1);

    coordinator.refresh(); // call 2: must be queued
    coordinator.refresh(); // call 3: still queued
    expect(fetchCalls).toBe(1);

    // Resolve first call
    resolveFirst!({ sessions: [createMockSession('s1')] });
    await Promise.resolve();

    // Now queued refresh runs
    expect(fetchCalls).toBe(2);
    coordinator.stop();
  });

  it('cancels superseded in-flight request when force=true', async () => {
    let aborted = false;
    const fetchFleet = vi.fn().mockImplementation((signal?: AbortSignal) => {
      if (signal) {
        signal.addEventListener('abort', () => { aborted = true; });
      }
      return new Promise(() => {}); // never resolves
    });

    const coordinator = new FleetRefreshCoordinator({
      fetchFleet,
      subscribeSSE: () => () => {},
    });

    coordinator.refresh();
    expect(coordinator.getState().inFlight).toBe(true);
    expect(aborted).toBe(false);

    coordinator.refresh(true); // force cancel
    expect(aborted).toBe(true);
    coordinator.stop();
  });

  it('computes bounded exponential backoff with jitter', () => {
    const coordinator = new FleetRefreshCoordinator({
      basePollIntervalMs: 1000,
      maxBackoffMs: 8000,
      backoffFactor: 2.0,
      jitterFactor: 0.25,
      random: () => 0.5,
    });

    // 0 failures (idle poll)
    expect(coordinator.computeBackoff(0)).toBe(1000);

    // 1 failure: base * 2 = 2000, jitter = 2000 * 0.25 * 0.5 = 250 -> 2250
    expect(coordinator.computeBackoff(1)).toBe(2250);

    // 2 failures: base * 4 = 4000, jitter = 4000 * 0.25 * 0.5 = 500 -> 4500
    expect(coordinator.computeBackoff(2)).toBe(4500);

    // 10 failures: clamped by maxBackoffMs (8000 + 1000 max bound)
    expect(coordinator.computeBackoff(10)).toBeLessThanOrEqual(9000);
  });

  it('applies SSE snapshots, stamps lastCompleteAt, and resets failures', () => {
    let sseCallback: (sessions: Session[]) => void;
    const nowStamp = new Date(2026, 9, 10, 10, 0, 0);

    const coordinator = new FleetRefreshCoordinator({
      subscribeSSE: (onData) => {
        sseCallback = onData;
        return () => {};
      },
      now: () => nowStamp,
    });

    coordinator.start();
    expect(coordinator.getState().lastCompleteAt).toBeNull();

    sseCallback!([createMockSession('agent-sse')]);
    const state = coordinator.getState();
    expect(state.status).toBe('live');
    expect(state.sessions).toHaveLength(1);
    expect(state.sessions[0].id).toBe('agent-sse');
    expect(state.lastCompleteAt).toBe(nowStamp);
    expect(state.failures).toBe(0);

    coordinator.stop();
  });
});
