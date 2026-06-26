import { defineConfig } from 'astro/config';
import react from '@astrojs/react';

const DAEMON = 'http://127.0.0.1:8765';
const proxy = (path) => ({ [path]: { target: DAEMON, changeOrigin: true } });

// In dev, forward API + SSE to the running daemon so the browser stays
// same-origin (matches prod where the daemon serves this build).
export default defineConfig({
  output: 'static',
  outDir: './dist',
  integrations: [react()],
  server: { port: 4321 },
  vite: {
    server: {
      proxy: {
        // The whole data/action API (REST + the /events/stream SSE + the
        // /sessions/{id}/attach WS) lives under /api/v1; one prefix covers it
        // all (and /api/docs). /healthz is the only API surface left at root.
        ...proxy('/api'),
        ...proxy('/healthz'),
      },
    },
  },
});
