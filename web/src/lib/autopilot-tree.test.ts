import { describe, it, expect } from 'vitest';
import { sessionRunID, isAutopilotOwned, sessionSlot, buildRunTree } from './autopilot-tree';
import type { Session } from './types';
import type { AutopilotRun } from './api';

function sess(over: Partial<Session>): Session {
  return {
    id: 'x', type: '', ticket: '', tmux_session: '', repo: '', worktree: '',
    branch: '', pr: '', prompt: '', workdir: '', subject: '', status: 'working',
    pid: 0, created_at: '', updated_at: '', events: null, last_pane_excerpt: '',
    supervised: false, ...over,
  };
}

function run(over: Partial<AutopilotRun> = {}): AutopilotRun {
  return {
    run_id: 'ap-1', name: 'release', plan_file: 'plans/release.yaml', repo: '/repo',
    state: 'active', gate: 'ci', brain: null, workers_in_flight: 0,
    tasks: { pending: 0, in_progress: 0, landed: 0, failed: 0 },
    backoff: null, landed_total: 0, plan_tasks: [], ...over,
  };
}

describe('sessionRunID / isAutopilotOwned', () => {
  it('prefers the back-ref field over tags', () => {
    const s = sess({ autopilot_run_id: 'ap-new', tags: ['run:ap-legacy'] });
    expect(sessionRunID(s)).toBe('ap-new');
    expect(isAutopilotOwned(s)).toBe(true);
  });
  it('falls back to run: and autopilot-run: tags', () => {
    expect(sessionRunID(sess({ tags: ['autopilot', 'run:ap-1'] }))).toBe('ap-1');
    expect(sessionRunID(sess({ tags: ['system:true', 'autopilot-run:ap-9'] }))).toBe('ap-9');
    expect(isAutopilotOwned(sess({ tags: ['run:ap-1'] }))).toBe(true);
  });
  it('treats a slot without run id as owned', () => {
    expect(isAutopilotOwned(sess({ autopilot_slot: 'worker' }))).toBe(true);
  });
  it('leaves ordinary agents unowned', () => {
    expect(sessionRunID(sess({ id: 'plain' }))).toBe('');
    expect(isAutopilotOwned(sess({ id: 'plain' }))).toBe(false);
  });
});

describe('buildRunTree', () => {
  it('matches the §4 shape and orders workers by ledger state', () => {
    const r = run({
      guardian_id: 'grd-1',
      brain: { agent_id: 'mgr-1', backend: 'claude', tier: 'pro', last_heartbeat: '', context_level: 'ok' },
      plan_tasks: [{ id: 'docs', prompt: 'Docs', after: [], status: 'pending' }, { id: 'ship', prompt: 'Ship', after: [], status: 'active' }],
      ledger_tasks: [{ id: 'docs', state: 'pending' }, { id: 'ship', state: 'in_progress' }],
    });
    const sessions = [
      sess({ id: 'w-ship', autopilot_run_id: 'ap-1', autopilot_slot: 'worker', autopilot_task_id: 'ship' }),
      sess({ id: 'mgr-1', role: 'autopilot', tags: ['run:ap-1'] }),
      sess({ id: 'grd-1', tags: ['system:true', 'autopilot-run:ap-1'] }),
      sess({ id: 'w-docs', autopilot_run_id: 'ap-1', autopilot_slot: 'worker', autopilot_task_id: 'docs' }),
      sess({ id: 'plain' }),
    ];
    const tree = buildRunTree(r, sessions);
    expect(tree.managers.map((s) => s.id)).toEqual(['mgr-1']);
    expect(tree.guardians.map((s) => s.id)).toEqual(['grd-1']);
    expect(tree.workerGroups.map((g) => `${g.state}:${g.taskId}`)).toEqual(['pending:docs', 'in_progress:ship']);
    expect(tree.workerGroups[0].sessions.map((s) => s.id)).toEqual(['w-docs']);
    expect(tree.workerGroups[1].sessions.map((s) => s.id)).toEqual(['w-ship']);
  });

  it('infers slots from tags when back-refs are empty', () => {
    const r = run({ guardian_id: 'g1', brain: { agent_id: 'b1', backend: 'claude', tier: '', last_heartbeat: '', context_level: '' } });
    expect(sessionSlot(sess({ id: 'b1', tags: ['run:ap-1'] }), r)).toBe('autopilot');
    expect(sessionSlot(sess({ id: 'g1', tags: ['autopilot-run:ap-1'] }), r)).toBe('guardian');
    expect(sessionSlot(sess({ id: 'w1', tags: ['run:ap-1'] }), r)).toBe('worker');
  });
});
