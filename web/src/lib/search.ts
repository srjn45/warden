import type { Session } from './types';

// sessionHaystack concatenates a session's searchable text — name, id, ticket,
// type, subject, prompt, branch, last pane excerpt — lowercased. Mirrors the
// daemon's sessionHaystack so client-side and `warden search` agree on what
// "matches".
export function sessionHaystack(s: Session): string {
  return [
    s.name ?? '', s.id, s.ticket, s.type, s.subject,
    s.prompt, s.branch, s.last_pane_excerpt,
  ].join('\n').toLowerCase();
}

// filterSessions returns every session matching the query. The query is split
// on whitespace into terms; every term must appear somewhere in the session's
// haystack (AND semantics). A blank query returns the input unchanged so an
// empty search bar shows the full fleet rather than nothing.
export function filterSessions(sessions: Session[], query: string): Session[] {
  const terms = query.toLowerCase().split(/\s+/).filter(Boolean);
  if (terms.length === 0) return sessions;
  return sessions.filter((s) => {
    const hay = sessionHaystack(s);
    return terms.every((t) => hay.includes(t));
  });
}
