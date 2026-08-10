import type { Session } from './types';
import { sourceDir, baseName, UNKNOWN_DIR } from './group';

// A session's kind discriminates a plain terminal from an AI agent. The daemon
// sends `kind` json-omitempty, so an agent omits the field entirely — absent or
// '' both mean 'agent'. Only an explicit 'terminal' is a terminal. This mirrors
// store.Session.IsTerminal() on the daemon side.
export function isTerminal(s: Session): boolean {
  return s.kind === 'terminal';
}

// partitionByKind splits a live session list into agents and terminals in a
// single order-preserving pass. Agent-centric surfaces (the cockpit grid, fleet
// counts, attention, pinned tabs) consume `agents`; the Terminals tab consumes
// `terminals`. Keeping the split in one place is what stops terminals from
// leaking back into the agent views now that both kinds ride the one SSE stream.
export function partitionByKind(sessions: Session[]): { agents: Session[]; terminals: Session[] } {
  const agents: Session[] = [];
  const terminals: Session[] = [];
  for (const s of sessions) (isTerminal(s) ? terminals : agents).push(s);
  return { agents, terminals };
}

// terminalName is the display handle for a terminal in the Terminals list. It
// mirrors the TUI's terminalDisplayName: prefer the explicit session name, else
// the working directory's base name, with the branch appended when known. Falls
// back to the id so a terminal is never nameless.
export function terminalName(s: Session): string {
  if (s.name && s.name.trim() !== '') return s.name;
  const dir = sourceDir(s);
  const base = dir === UNKNOWN_DIR ? '' : baseName(dir);
  const label = base || s.id;
  return s.branch ? `${label} (${s.branch})` : label;
}
