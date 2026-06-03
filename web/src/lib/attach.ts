// attachURL builds the WebSocket URL for an agent's interactive attach endpoint,
// matching the page's scheme (ws for http, wss for https).
export function attachURL(loc: { protocol: string; host: string }, id: string): string {
  const scheme = loc.protocol === 'https:' ? 'wss:' : 'ws:';
  return `${scheme}//${loc.host}/sessions/${encodeURIComponent(id)}/attach`;
}

// resizeMessage is the text control frame announcing terminal dimensions.
export function resizeMessage(cols: number, rows: number): string {
  return JSON.stringify({ cols, rows });
}
