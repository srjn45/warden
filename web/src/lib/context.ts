// Context-size gauge helpers for the web UI. Mirrors the daemon's gauge: a
// short "210k" figure plus a CSS class tied to the ok/warning/critical band.

export function fmtTokens(tokens?: number): string {
  if (!tokens || tokens <= 0) return '—';
  return `${Math.round(tokens / 1000)}k`;
}

export function contextClass(state?: string): string {
  switch (state) {
    case 'warning': return 'ctx-warning';
    case 'critical': return 'ctx-critical';
    case 'ok': return 'ctx-ok';
    default: return 'ctx-unknown';
  }
}

// known reports whether the agent has a usable gauge yet (a model turn ran).
export function known(tokens?: number, state?: string): boolean {
  return !!state && state !== '' && (tokens ?? 0) > 0;
}
