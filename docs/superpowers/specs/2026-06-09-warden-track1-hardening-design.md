# warden Track-1 security hardening — design

**Date:** 2026-06-09
**Status:** Approved for planning
**Feature:** Three small, independent hardening fixes for the now-public warden daemon: tighten data-directory permissions (`0o755 → 0o700`), add slowloris/body-size HTTP guards that don't break long-lived streams, and refuse a non-loopback bind by default (the daemon has no auth).

## Motivation

warden is now public ([[warden-published-github]]) and the daemon serves an **unauthenticated** local HTTP + WebSocket API. Three low-effort, high-value gaps:

1. **World-traversable data dirs.** `~/.warden/{sessions,closed,pipelines,context,inbox}` are created `0o755` and the `prompts` dir via shell `mkdir -p` (umask default). The session JSON *files* are already `0o600` (Go's `os.CreateTemp` in `atomicWriteJSON`), so file *contents* aren't world-readable — but `0o755` dirs let any local user **enumerate session IDs and the data-dir structure**, and the `prompts` dir (which can contain tokens passed in prompts) isn't even guaranteed `0o600` per-file. Defense in depth: the data dirs should be owner-only.
2. **No HTTP server timeouts / body cap.** The `http.Server` is `{Addr, Handler}` with no `ReadHeaderTimeout`, `IdleTimeout`, `MaxHeaderBytes`, and handlers decode request bodies with no size limit. A slow or oversized client can tie up the daemon.
3. **Unvalidated bind address.** `WARDEN_ADDR` is taken verbatim; a misconfiguration to `0.0.0.0:8765` (or `:8765`) silently exposes the auth-less daemon to the network.

## Goals

- All warden data directories are `0o700`, both newly created and **already-existing** ones (tighten current installs, not just fresh).
- The daemon resists slowloris (slow headers), unbounded request bodies, and slow non-streaming handlers — **without** breaking the three long-lived endpoints (SSE, WS attach, message long-poll).
- A non-loopback bind is refused by default, with an explicit env opt-in for anyone who genuinely wants it.

## Non-goals

- Authentication / TLS for the daemon (remains out of scope; localhost-only is the security model).
- Encrypting data at rest.
- Per-file permission audits beyond the directory fix (files are already `0o600`).
- Rate limiting, request tracing, security headers (separate, larger efforts).

## Unit 1 — Directory permissions (`0o755 → 0o700`)

**Two parts: correct creation, and tighten existing.**

**a) Creation sites → `0o700`** (so dirs are correct even outside the daemon, e.g. tests / direct store use):
- `internal/store/file.go:41,44` — `sessions`, `closed`.
- `internal/ctxstore/ctxstore.go:56` — context KV dir.
- `internal/mailbox/mailbox.go:41` — inbox dir.
- `internal/pipeline/store.go:27` — pipelines dir.
- `internal/lifecycle/lifecycle.go:580,997` — the `prompts` dir, created via the runner as `mkdir -p <PromptsDir>`. Change to `mkdir -m 700 -p <PromptsDir>` (keeps the `Runner` seam intact for tests; the parent `DataDir` is already `0o700` from the store, so `-m 700` on the final component suffices). The `prompts` dir is created **lazily on first spawn**, so it may not exist when the startup chmod (below) runs — this creation-time mode is what guarantees it.
- `internal/metrics/recorder.go` is already `0o700` (reference; no change).

**b) Tighten existing → new `daemon.HardenDataDir(dataDir string) error`** (pure, unit-tested):
- `chmod 0o700` on `dataDir` itself plus each known subdir — `sessions, closed, context, inbox, pipelines, prompts, metrics` — that **already exists**; silently skip any that are missing; return the first real error.
- Called once at daemon startup from `internal/cli/daemon.go`, after the stores are constructed (so their dirs exist).

## Unit 2 — HTTP server hardening

**a) `http.Server` fields** (`internal/daemon/server.go`, where `httpSrv := &http.Server{Addr: addr, Handler: s.router()}`):
- `ReadHeaderTimeout = 10 * time.Second` — bounds slowloris header attacks; does not affect body streaming or response writes.
- `IdleTimeout = 120 * time.Second` — keep-alive idle cap; safe.
- `MaxHeaderBytes = 1 << 20`.
- **Leave `ReadTimeout` and `WriteTimeout` at 0 (unbounded)** — a global `WriteTimeout` would kill SSE (`/events/stream`), the WS `tmux attach` (`/sessions/{id}/attach`), and the message long-poll (`/sessions/{id}/messages/wait`).

**b) New `internal/daemon/middleware.go`** with two middlewares + a predicate, registered in `router()` right after `recoverMiddleware`:
- `maxBytesMiddleware(n int64)` — wraps `r.Body = http.MaxBytesReader(w, r.Body, n)` for every request. Harmless on GETs (no body read) and on the WS upgrade (the connection is hijacked before any body read); caps JSON POSTs. `n = 1 << 20`.
- `isStreamingPath(r *http.Request) bool` (pure, unit-tested): true when `r.URL.Path == "/events/stream"` or it ends with `/attach` or `/messages/wait`.
- `writeTimeoutMiddleware(d time.Duration)` — wraps the handler in `http.TimeoutHandler(next, d, body)` but **bypasses** `isStreamingPath` requests (calling `next` directly), because `http.TimeoutHandler` buffers the response and its wrapped `ResponseWriter` implements neither `http.Flusher` nor `http.Hijacker` — it would break SSE and the WS upgrade. `d = 30 * time.Second`. The timeout body is the standard error envelope `{"error":"request timed out"}`.

This keeps the route tree untouched: one global middleware that self-excludes the three streaming paths, rather than splitting routes into timed/untimed groups.

## Unit 3 — Non-loopback bind guard

**a) `internal/config/config.go`:**
- Add `Config.AllowNonLoopback bool`, parsed from `WARDEN_ALLOW_NONLOOPBACK` (legacy `AGENTCTL_ALLOW_NONLOOPBACK`) — **off** by default, on only for `1/on/true` (same shape as `notifyEnabled`).
- Add pure `IsLoopbackHost(addr string) bool` (unit-tested), using `net` (no DNS):
  - `net.SplitHostPort(addr)`; on error treat the whole string as the host.
  - empty host (`:8765`) → **false** (binds all interfaces — dangerous).
  - `"localhost"` → true; otherwise `net.ParseIP(host).IsLoopback()`; an unresolvable hostname → false (fail safe).

**b) `internal/cli/daemon.go` `RunE`:** after `cfg` is loaded and any `--addr` override applied, before `ListenAndServe`:
```go
if !config.IsLoopbackHost(cfg.Addr) && !cfg.AllowNonLoopback {
    return fmt.Errorf("refusing to bind non-loopback address %q: the warden daemon has no authentication; set WARDEN_ALLOW_NONLOOPBACK=1 to override", cfg.Addr)
}
```
Fails fast, before binding.

## Testing

- **`config`**: `TestIsLoopbackHost` (table: `127.0.0.1:8765`✓, `localhost:8765`✓, `[::1]:8765`✓, `:8765`✗, `0.0.0.0:8765`✗, `192.168.1.5:8765`✗, bare `127.0.0.1`✓); `TestAllowNonLoopbackFlag` (default off; `WARDEN_ALLOW_NONLOOPBACK=1` on).
- **`daemon` HardenDataDir**: build a temp tree with some subdirs at `0o755` and one absent; assert present dirs become `0o700`, the missing one is skipped, no error.
- **`daemon` middleware**: `isStreamingPath` table; `writeTimeoutMiddleware` via `httptest` — a fast handler returns 200, a slow non-streaming handler returns 503 (timeout), and a slow handler on a streaming path is **not** timed out (completes); `maxBytesMiddleware` — a request body over the cap makes the handler's read/decode fail.
- **store dir mode**: one assertion (in the store test) that `NewFileStore` creates `sessions`/`closed` at `0o700`.
- The bind-guard enforcement in `RunE` is a two-line check over the unit-tested `IsLoopbackHost`; verified manually (start with `WARDEN_ADDR=0.0.0.0:8799` → refused; with the override → starts).

## Out of scope / future

- Auth/TLS, rate limiting, security headers, request tracing — larger Track-1+/Track-4 items noted in the original assessment.
- Tightening individual file modes (already `0o600` via `atomicWriteJSON`).
