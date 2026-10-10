# Agent-store lock contention: incident, migration, rollback, runbook

## Incident
Under load, a slow integrity scan or a stalled write held the single store mutex.
Every reader (REST, SSE, CLI, TUI, Web Cockpit) queued behind it, and cockpit
pollers issued overlapping refreshes, so requests piled up and the fleet view
froze or timed out.

## What changed
- **Snapshot read model** — reads (`List`, `Get`, `ListClosed`, …) serve an
  immutable snapshot and never take the write slot or scan.
- **Auditor isolation** — integrity audits run off the write path.
- **ctx-aware writer gate** — queued writers honour context
  cancel/deadline and abandon cleanly (`ctx_abandoned` metric).
- **Cockpit refresh coalescing** — one in-flight refresh per surface,
  superseded ticks cancelled, SSE-driven invalidation, jittered backoff, and a
  "last complete snapshot" banner when degraded.
- **`Store.Diagnostics()`** — lock waiters, holder op, held ms, slowest holds,
  per-op p50/p99, wait/hold p99, abandoned counts, snapshot version/age, audit
  report.

## Verification
- `internal/agentstore/adversarial_test.go`: blocked write (no pile-up, bounded
  reader latency, queued-writer deadlines, no data loss, visible metrics),
  delayed audit scan isolation, concurrent readers/writers with pre-cancelled
  writes and close/reopen recovery.
- Contention contract/baseline tests and refresh tests (Go `internal/tui`,
  web `refresh.test.ts`).
- Run: `go test -race ./...` and the web test suite.

## Migration
No data or schema migration. Deploy the new binary and restart the daemon; the
snapshot is built from the existing store at open. No config changes needed.

## Rollback
Redeploy the previous release and restart the daemon. On-disk format is
unchanged, so no data conversion is required.

## Operator runbook
1. **Symptom**: cockpit shows the "last complete snapshot" banner or slow
   fleet refresh.
2. **Inspect** `Store.Diagnostics()`: `holder_op` + `held_ms` identify a stuck
   writer; `lock_waiters` shows queue depth; `slowest_holds` shows history;
   `snapshot_age_ms` shows read staleness; `audit` shows scan state.
3. **Stuck writer**: callers with contexts abandon on their own; if `held_ms`
   keeps growing, restart the daemon — acknowledged writes are durable.
4. **Audit findings** (`state` suspect/degraded): follow the agent-store
   integrity runbook; reads continue from the last good snapshot.
5. **Known gap**: HTTP/Prometheus exposition of these metrics (spec §9) is not
   yet implemented; diagnostics are in-process only.
