export interface PressureStatus {
  level: number;
  level_name: string;
  agent_count: number;
  max_agents: number;
  elevated: boolean;
  gate_enabled: boolean;
}

export interface Verdict {
  elevated: boolean;
  level: number;
  agent_count: number;
  max_agents: number;
  reason: string;
}

// gaugeClass maps a level to a CSS severity class for the gauge chip.
export function gaugeClass(level: number): 'ok' | 'warn' | 'crit' {
  if (level >= 4) return 'crit';
  if (level >= 2) return 'warn';
  return 'ok';
}

// gaugeLabel renders the always-on gauge text, e.g. "pressure: warn · 6 agents".
export function gaugeLabel(p: PressureStatus): string {
  return `pressure: ${p.level_name} · ${p.agent_count} agent${p.agent_count === 1 ? '' : 's'}`;
}
