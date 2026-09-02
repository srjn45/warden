import type { AutopilotRun } from './api';
import type { Session } from './types';

export const LEDGER_STATES = ['pending', 'assigned', 'in_progress', 'pr_open', 'gated', 'landed'] as const;
export type LedgerState = (typeof LEDGER_STATES)[number];
export type AutopilotSlot = 'autopilot' | 'guardian' | 'worker';

export function sessionRunID(s: Session): string {
  if (s.autopilot_run_id) return s.autopilot_run_id;
  for (const tag of s.tags ?? []) {
    if (tag.startsWith('run:')) return tag.slice(4);
    if (tag.startsWith('autopilot-run:')) return tag.slice('autopilot-run:'.length);
  }
  return '';
}

export function isAutopilotOwned(s: Session): boolean {
  if (s.autopilot_run_id || s.autopilot_slot) return true;
  return sessionRunID(s) !== '';
}

export function sessionSlot(s: Session, run: AutopilotRun): AutopilotSlot {
  if (s.autopilot_slot) return s.autopilot_slot;
  if (run.guardian_id && s.id === run.guardian_id) return 'guardian';
  if (run.brain?.agent_id && s.id === run.brain.agent_id) return 'autopilot';
  if (s.role === 'autopilot') return 'autopilot';
  return 'worker';
}

export interface WorkerGroup {
  taskId: string;
  state: string;
  sessions: Session[];
}

export interface RunTree {
  managers: Session[];
  guardians: Session[];
  workerGroups: WorkerGroup[];
}

export function buildRunTree(run: AutopilotRun, sessions: Session[]): RunTree {
  const owned = sessions.filter((s) => sessionRunID(s) === run.run_id || s.id === run.guardian_id);
  const managers: Session[] = [];
  const guardians: Session[] = [];
  const workers: Session[] = [];
  for (const s of owned) {
    switch (sessionSlot(s, run)) {
      case 'autopilot': managers.push(s); break;
      case 'guardian': guardians.push(s); break;
      default: workers.push(s); break;
    }
  }
  return { managers, guardians, workerGroups: groupWorkersByLedger(workers, run) };
}

function groupWorkersByLedger(workers: Session[], run: AutopilotRun): WorkerGroup[] {
  const stateByTask: Record<string, string> = {};
  for (const t of run.ledger_tasks ?? []) {
    if (t.id) stateByTask[t.id] = t.state;
  }
  const byTask = new Map<string, Session[]>();
  const seen: string[] = [];
  for (const s of workers) {
    const tid = s.autopilot_task_id ?? '';
    if (!byTask.has(tid)) {
      seen.push(tid);
      byTask.set(tid, []);
    }
    byTask.get(tid)!.push(s);
  }

  const used = new Set<string>();
  const groups: WorkerGroup[] = [];
  const appendGroup = (id: string, state: string) => {
    if (used.has(id)) return;
    const sess = byTask.get(id);
    if (!sess || sess.length === 0) return;
    used.add(id);
    groups.push({ taskId: id, state, sessions: sess });
  };

  for (const st of LEDGER_STATES) {
    for (const t of run.ledger_tasks ?? []) {
      if (t.state === st) appendGroup(t.id, st);
    }
    for (const id of seen) {
      if (stateByTask[id] === st) appendGroup(id, st);
    }
  }
  for (const t of run.plan_tasks ?? []) appendGroup(t.id, stateByTask[t.id] ?? '');
  for (const id of seen) appendGroup(id, stateByTask[id] ?? '');
  return groups;
}
