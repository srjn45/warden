import { describe, it, expect } from 'vitest';
import { busyIdle } from './status';

describe('busyIdle', () => {
  it('maps working/spawning to busy', () => {
    expect(busyIdle('working')).toEqual({ label: 'Busy', kind: 'busy' });
    expect(busyIdle('spawning')).toEqual({ label: 'Starting', kind: 'busy' });
  });
  it('maps waiting_for_input to attention', () => {
    expect(busyIdle('waiting_for_input')).toEqual({ label: 'Needs input', kind: 'attention' });
  });
  it('maps idle/done to idle', () => {
    expect(busyIdle('idle')).toEqual({ label: 'Idle', kind: 'idle' });
    expect(busyIdle('done')).toEqual({ label: 'Done', kind: 'idle' });
  });
  it('maps errored/orphaned to error', () => {
    expect(busyIdle('errored')).toEqual({ label: 'Error', kind: 'error' });
    expect(busyIdle('orphaned')).toEqual({ label: 'Orphaned', kind: 'error' });
  });
  it('shows the exit code on an errored badge when present', () => {
    expect(busyIdle('errored', 137).label).toBe('Error (137)');
  });
  it('errored without a code keeps the plain Error label', () => {
    expect(busyIdle('errored').label).toBe('Error');
  });
  it('errored with code 0 keeps the plain Error label', () => {
    expect(busyIdle('errored', 0).label).toBe('Error');
  });
});
