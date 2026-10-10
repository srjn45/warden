import type { Session, AutopilotStatus } from './types';
import type { ProjectTree } from './tree';
import { listSessions, getTree, subscribeSessions, ApiError } from './api';

export type FleetStatus = 'live' | 'stale' | 'degraded' | 'disconnected' | 'connecting';

export interface FleetRefreshState {
  sessions: Session[];
  tree: ProjectTree | null;
  treeError: string | null;
  status: FleetStatus;
  lastCompleteAt: Date | null;
  inFlight: boolean;
  failures: number;
}

export interface BannerInfo {
  show: boolean;
  text: string;
  severity: 'error' | 'warning' | 'info';
}

export interface CoordinatorOptions {
  basePollIntervalMs?: number;
  conservativeIntervalMs?: number;
  maxBackoffMs?: number;
  backoffFactor?: number;
  jitterFactor?: number;
  fetchFleet?: (signal?: AbortSignal) => Promise<{ sessions: Session[]; tree?: ProjectTree }>;
  subscribeSSE?: (
    onData: (sessions: Session[]) => void,
    onError: () => void,
    onOpen: () => void,
    onAutopilot?: (status: AutopilotStatus) => void,
    onTree?: (tree: ProjectTree) => void,
  ) => () => void;
  now?: () => Date;
  random?: () => number;
}

export function formatTime(d: Date): string {
  const pad = (n: number) => String(n).padStart(2, '0');
  return `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
}

export function formatFleetBanner(state: FleetRefreshState): BannerInfo {
  if (state.status === 'live') {
    return { show: false, text: '', severity: 'info' };
  }

  const stamp = state.lastCompleteAt
    ? ` · showing last complete fleet from ${formatTime(state.lastCompleteAt)}`
    : '';

  switch (state.status) {
    case 'degraded':
      return {
        show: true,
        text: `session store degraded — run \`warden doctor\`${stamp}`,
        severity: 'warning',
      };
    case 'disconnected':
      return {
        show: true,
        text: stamp
          ? `daemon not running — start it with \`warden daemon\`${stamp}`
          : 'daemon not running — start it with `warden daemon`',
        severity: 'error',
      };
    case 'stale':
      return {
        show: true,
        text: `daemon not responding — request timed out${stamp}`,
        severity: 'warning',
      };
    case 'connecting':
    default:
      if (!stamp) {
        return { show: false, text: '', severity: 'info' };
      }
      return {
        show: true,
        text: `reconnecting to daemon${stamp}`,
        severity: 'warning',
      };
  }
}

export class FleetRefreshCoordinator {
  private basePollIntervalMs: number;
  private conservativeIntervalMs: number;
  private maxBackoffMs: number;
  private backoffFactor: number;
  private jitterFactor: number;
  private fetchFleetImpl: (signal?: AbortSignal) => Promise<{ sessions: Session[]; tree?: ProjectTree }>;
  private subscribeSSEImpl: (
    onData: (sessions: Session[]) => void,
    onError: () => void,
    onOpen: () => void,
    onAutopilot?: (status: AutopilotStatus) => void,
    onTree?: (tree: ProjectTree) => void,
  ) => () => void;
  private now: () => Date;
  private random: () => number;

  private state: FleetRefreshState = {
    sessions: [],
    tree: null,
    treeError: null,
    status: 'connecting',
    lastCompleteAt: null,
    inFlight: false,
    failures: 0,
  };

  private listeners: Set<(state: FleetRefreshState) => void> = new Set();
  private sseUnsub: (() => void) | null = null;
  private pollTimer: ReturnType<typeof setTimeout> | null = null;
  private activeAbort: AbortController | null = null;
  private queued = false;
  private stopped = false;
  private sseConnected = false;

  constructor(opts: CoordinatorOptions = {}) {
    this.basePollIntervalMs = opts.basePollIntervalMs ?? 2000;
    this.conservativeIntervalMs = opts.conservativeIntervalMs ?? 15000;
    this.maxBackoffMs = opts.maxBackoffMs ?? 15000;
    this.backoffFactor = opts.backoffFactor ?? 1.5;
    this.jitterFactor = opts.jitterFactor ?? 0.25;
    this.now = opts.now ?? (() => new Date());
    this.random = opts.random ?? (() => Math.random());

    this.fetchFleetImpl = opts.fetchFleet ?? (async (signal?: AbortSignal) => {
      const [sessions, tree] = await Promise.all([
        listSessions(signal),
        getTree(signal).catch(() => null),
      ]);
      return { sessions, tree: tree ?? undefined };
    });

    this.subscribeSSEImpl = opts.subscribeSSE ?? subscribeSessions;
  }

  public getState(): FleetRefreshState {
    return this.state;
  }

  public subscribe(listener: (state: FleetRefreshState) => void): () => void {
    this.listeners.add(listener);
    listener(this.state);
    return () => this.listeners.delete(listener);
  }

  private notify() {
    for (const l of this.listeners) {
      l(this.state);
    }
  }

  public computeBackoff(failures: number): number {
    if (failures <= 0) {
      return this.sseConnected ? this.conservativeIntervalMs : this.basePollIntervalMs;
    }
    const base = this.basePollIntervalMs;
    const exp = Math.min(this.maxBackoffMs, base * Math.pow(this.backoffFactor, failures));
    const jitter = exp * this.jitterFactor * this.random();
    return Math.min(this.maxBackoffMs + 1000, exp + jitter);
  }

  public start(onAutopilot?: (status: AutopilotStatus) => void) {
    if (this.stopped) return;

    this.sseUnsub = this.subscribeSSEImpl(
      (sessions) => this.onSSESessions(sessions),
      () => this.onSSEError(),
      () => this.onSSEOpen(),
      onAutopilot,
      (tree) => this.onSSETree(tree),
    );

    // Initial immediate fleet refresh.
    this.refresh();
  }

  public stop() {
    this.stopped = true;
    if (this.sseUnsub) {
      this.sseUnsub();
      this.sseUnsub = null;
    }
    if (this.pollTimer) {
      clearTimeout(this.pollTimer);
      this.pollTimer = null;
    }
    if (this.activeAbort) {
      this.activeAbort.abort();
      this.activeAbort = null;
    }
  }

  private onSSEOpen() {
    this.sseConnected = true;
    this.state = {
      ...this.state,
      failures: 0,
      status: this.state.lastCompleteAt ? 'live' : this.state.status,
    };
    this.notify();
    this.scheduleFallbackPoll();
  }

  private onSSESessions(sessions: Session[]) {
    this.sseConnected = true;
    this.state = {
      ...this.state,
      sessions,
      status: 'live',
      lastCompleteAt: this.now(),
      failures: 0,
    };
    this.notify();
    this.scheduleFallbackPoll();
  }

  private onSSETree(tree: ProjectTree) {
    this.state = {
      ...this.state,
      tree,
      treeError: null,
    };
    this.notify();
  }

  private onSSEError() {
    this.sseConnected = false;
    this.state = {
      ...this.state,
      failures: this.state.failures + 1,
      status: this.state.status === 'live' ? 'stale' : this.state.status,
    };
    this.notify();
    // Invalidate and trigger conservative fallback poll.
    this.scheduleFallbackPoll(true);
  }

  public refresh(force = false) {
    if (this.stopped) return;

    if (this.state.inFlight) {
      this.queued = true;
      if (force && this.activeAbort) {
        this.activeAbort.abort();
      }
      return;
    }

    this.state = { ...this.state, inFlight: true };
    this.notify();

    this.activeAbort = new AbortController();
    const signal = this.activeAbort.signal;

    this.fetchFleetImpl(signal)
      .then((data) => {
        if (signal.aborted || this.stopped) return;
        this.state = {
          ...this.state,
          sessions: data.sessions,
          tree: data.tree ?? this.state.tree,
          treeError: null,
          status: 'live',
          lastCompleteAt: this.now(),
          failures: 0,
          inFlight: false,
        };
        this.notify();
        this.onRefreshComplete();
      })
      .catch((err) => {
        if (signal.aborted || this.stopped) return;
        const failures = this.state.failures + 1;
        let nextStatus: FleetStatus = 'disconnected';
        if (err instanceof ApiError && err.status === 503) {
          nextStatus = 'degraded';
        } else if (err instanceof ApiError && err.status >= 500) {
          nextStatus = 'stale';
        } else if (err?.name === 'AbortError') {
          return;
        }

        this.state = {
          ...this.state,
          failures,
          status: nextStatus,
          inFlight: false,
        };
        this.notify();
        this.onRefreshComplete();
      });
  }

  private onRefreshComplete() {
    this.activeAbort = null;
    if (this.queued) {
      this.queued = false;
      this.refresh();
    } else {
      this.scheduleFallbackPoll();
    }
  }

  private scheduleFallbackPoll(immediate = false) {
    if (this.stopped) return;
    if (this.pollTimer) {
      clearTimeout(this.pollTimer);
      this.pollTimer = null;
    }

    const delay = immediate ? 0 : this.computeBackoff(this.state.failures);
    this.pollTimer = setTimeout(() => {
      this.refresh();
    }, delay);
  }
}
