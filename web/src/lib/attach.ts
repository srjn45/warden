import { withToken, API_PREFIX } from './token';

// attachURL builds the WebSocket URL for an agent's interactive attach endpoint,
// matching the page's scheme (ws for http, wss for https). A bearer token, when
// set, rides along as a ?token= query param since WebSocket can't set headers.
export function attachURL(loc: { protocol: string; host: string }, id: string): string {
  const scheme = loc.protocol === 'https:' ? 'wss:' : 'ws:';
  return withToken(`${scheme}//${loc.host}${API_PREFIX}/sessions/${encodeURIComponent(id)}/attach`);
}

// cockpitAttachURL builds the WebSocket URL for the daemon-owned web cockpit
// (the "TUI" tab) — the shared three-pane dashboard, not a single agent. Same
// scheme/token handling as attachURL.
export function cockpitAttachURL(loc: { protocol: string; host: string }): string {
  const scheme = loc.protocol === 'https:' ? 'wss:' : 'ws:';
  return withToken(`${scheme}//${loc.host}${API_PREFIX}/cockpit/attach`);
}

// resizeMessage is the text control frame announcing terminal dimensions.
export function resizeMessage(cols: number, rows: number): string {
  return JSON.stringify({ cols, rows });
}
