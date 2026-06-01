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
});
