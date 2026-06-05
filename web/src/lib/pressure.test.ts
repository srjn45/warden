import { describe, it, expect } from 'vitest';
import { gaugeClass, gaugeLabel } from './pressure';

describe('gaugeClass', () => {
  it('maps levels to severity', () => {
    expect(gaugeClass(1)).toBe('ok');
    expect(gaugeClass(2)).toBe('warn');
    expect(gaugeClass(4)).toBe('crit');
  });
});

describe('gaugeLabel', () => {
  it('pluralizes agents', () => {
    expect(gaugeLabel({ level: 2, level_name: 'warn', agent_count: 6, max_agents: 5, elevated: true, gate_enabled: true }))
      .toBe('pressure: warn · 6 agents');
    expect(gaugeLabel({ level: 1, level_name: 'normal', agent_count: 1, max_agents: 5, elevated: false, gate_enabled: true }))
      .toBe('pressure: normal · 1 agent');
  });
});
