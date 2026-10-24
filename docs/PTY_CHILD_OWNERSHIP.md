# PTY child ownership and bounded cleanup

Production sessions intentionally survive the daemon. `ptyholder.Launch` starts
a detached holder; the holder owns the PTY master and reaps its command. The
daemon owns a duplicate master and an authenticated status subscription. On
shutdown `Session.Detach` closes only that duplicate and its subscription.
After a crash the kernel closes those descriptors. The holder keeps the shell
and process tree alive for authenticated re-adoption (PID, start time, peer UID,
credential and protocol checks). `deploy/warden.service.template` retains
`KillMode=process`, so a systemd restart signals the daemon, preserving holders
and tmux sessions. Changing this to a cgroup-wide kill breaks that contract.

| Event | Owned resources and disposition |
| --- | --- |
| Normal command exit | Its owner calls `Wait`. Runtime readers close their master on EOF; if descendants retain the slave, `DrainGrace` bounds draining and closes the runtime descriptor. The holder retains exit status for its configured linger (production default 24 hours). |
| Explicit session termination | Signal the live owned leader's process group, escalate after `KillGrace`, and bound the second wait. Close the master and subscriptions. Observe group reaping for one further `KillGrace`; report remaining members without a post-reap signal sweep. |
| Holder startup error/cancellation | Readiness has a 10 second ceiling or the caller's earlier deadline. Signal that exact launched holder with TERM; allow 3 seconds for cleanup, then kill that holder and count failure. The holder installs its TERM handler before launching the PTY command and cleans up on initialization errors. |
| Holder release | Authenticate to the holder and wait for cleanup completion, bounded by socket/request deadlines and a 3 second completion wait. Retain ownership records on failure. A failed release prevents replacement launch, except for an unreachable holder whose recorded holder and child incarnations are both verified gone (PID plus start time): that record is forgotten so an explicit restore can launch. |
| Daemon graceful stop/crash/systemd restart | Preserve production holders and tmux sessions; detach or lose only the daemon's descriptors. Re-adopt the same process, without replaying its task command. |
| Test failure or test process cancellation | Normal cleanup releases authenticated holders. Fixtures opt into `TestOwner` (PID plus start time), persisted in holder metadata; the holder checks it every 100ms and tears down when that incarnation disappears. Survival fixtures use the **outer test process**, so killing their simulated daemon still exercises real re-adoption. |
| PTY-host tmux fixture exit/SIGKILL | Each fixture uses a private socket and a random session ownership token. A separate test-binary guard holds the read end of a lease pipe; test process exit closes the write end. The guard lists session IDs and kills only matching tokens, within a 3 second budget. There is no `kill-server` call. |

Holder teardown gives its command 200ms after TERM, then up to one second after
KILL to be reaped. A missing/recycled leader is never permission to signal its
old process group. `KillAll` is a legacy name for authenticated, bounded cleanup
of a supplied holder root; it returns failures, preserves unresolved evidence,
and never signals PIDs merely because metadata names them. Test cleanup reports
those failures and retains its directory for diagnosis.

Holder requests are limited to 4KiB and 64 concurrent handlers. Authentication
reads have a 10 second deadline and writes have deadlines. Long-lived status
waits detect client disconnect, and shutdown closes active connections. Client
adopt/release requests have a 5 second I/O ceiling and honor earlier context
deadlines/cancellation. A status wait intentionally has no exit deadline;
`Abort` closes its connection on detach or cleanup timeout.

## Resource visibility

The existing `/api/v1/pty/metrics` and `.prom` endpoints expose these additional
process-local metrics (also available via the PTY metrics CLI/MCP):

| Metric | Meaning |
| --- | --- |
| `warden_pty_runtime_children` | Direct child leaders awaiting `Wait`, including failed starts. |
| `warden_pty_runtime_masters` | Open runtime master descriptors, including adopted duplicates. |
| `warden_pty_runtime_goroutines` | Active runtime reader, writer and waiter goroutines. |
| `warden_pty_holder_children` | Holder children launched by this daemon and awaiting `Wait`; deliberately preserved live holders remain counted. |
| `warden_pty_cleanup_failures_total` | Holder cleanup failures, failed-start/reap timeouts and unresolved runtime process groups. |
| `warden_pty_process_goroutines` | All daemon goroutines. |
| `warden_pty_process_open_fds` | All daemon descriptors, on Linux where `/proc/self/fd` is available. |

These counters reset with the daemon; holder-child counts do not pretend that
re-adopted holders are new local children. Runtime counters continue to reflect
adopted descriptors and workers. Unreaped local children remain visible rather
than being hidden when a cleanup deadline expires. There is no safe guarantee
that SIGKILL can reap an uninterruptible kernel task: timeouts report that case
and preserve identity evidence instead of widening the kill scope.

Regression coverage includes owner exit and PID reuse, cancelled readiness,
tampered child metadata, idempotent lost-reaper timeout accounting, normal-exit
descriptor/worker release, foreign tmux token preservation, resource soak, and
the existing daemon/holder survival and adoption fixtures. These run only against
isolated state directories, test processes and private tmux sockets.
