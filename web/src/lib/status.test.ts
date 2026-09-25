import { describe, it, expect } from 'vitest';
import { busyIdle } from './status';

describe('busyIdle', () => {
  it('maps working/spawning to busy', () => {
    expect(busyIdle('working')).toEqual({ label: 'busy', kind: 'busy' });
    expect(busyIdle('spawning')).toEqual({ label: 'pending', kind: 'busy' });
  });
  it('maps waiting_for_input to attention', () => {
    expect(busyIdle('waiting_for_input')).toEqual({ label: 'need-input', kind: 'attention' });
  });
  it('maps idle/done to idle', () => {
    expect(busyIdle('idle')).toEqual({ label: 'idle', kind: 'idle' });
    expect(busyIdle('done')).toEqual({ label: 'done', kind: 'idle' });
  });
  it('maps errored/orphaned to error', () => {
    expect(busyIdle('errored')).toEqual({ label: 'orphaned', kind: 'error' });
    expect(busyIdle('orphaned')).toEqual({ label: 'orphaned', kind: 'error' });
  });
  it('uses done when exit evidence is present', () => {
    expect(busyIdle('errored', 137).label).toBe('done');
  });
  it('errored without a code keeps the plain Error label', () => {
    expect(busyIdle('errored').label).toBe('orphaned');
  });
  it('uses done for a zero exit code', () => {
    expect(busyIdle('errored', 0).label).toBe('done');
  });
});

it('presents all seven states exactly', () => {
  expect(['spawning', 'working', 'idle', 'waiting_for_input', 'done', 'orphaned', 'rate_limited'].map(
    s => busyIdle(s as import('./types').Status).label,
  )).toEqual(['pending', 'busy', 'idle', 'need-input', 'done', 'orphaned', 'rate_limited']);
  expect(busyIdle('rate_limited').kind).toBe('attention');
});
