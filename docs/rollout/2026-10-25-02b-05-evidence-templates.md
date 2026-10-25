# PTY rollout evidence templates

Blank forms for [`../RUNBOOK-pty-rollout.md`](../RUNBOOK-pty-rollout.md) §3 and §11.
Copy one file per cohort/date. **Every value must be observed on a running
system.** A field you cannot fill stays `MISSING`; never estimate, back-fill from
tests, or copy fixture numbers. Fixture results (e.g.
[`2026-10-25-02b-05-t1-adversarial-matrix.md`](2026-10-25-02b-05-t1-adversarial-matrix.md))
are not evidence for any template here.

Current state of every field below: **MISSING** (no shadow baseline, direct cohort,
soak, or drain has been observed).

## A. Shadow baseline

| Field | Value |
|---|---|
| Operator | MISSING |
| Date / window | MISSING |
| `wd version` + source SHA of the running daemon | MISSING |
| `runtime.pty` | MISSING |
| Sessions observed (terminals / agents) | MISSING |
| Poller status accuracy | MISSING |
| Error rate | MISSING |
| RSS / FD / goroutine per session (daemon) | MISSING |

## B. Cohort authorization

| Field | Value |
|---|---|
| Authorizing operator (human) | MISSING |
| Authorization date and written reference | MISSING |
| Cohort name and scope (kinds: terminals only unless agent gate exists) | MISSING |
| Window start / end | MISSING |
| Deployed version + SHA (from the running daemon) | MISSING |
| Agents included? (must be **No** today) | MISSING |
| Rollback owner and command | MISSING |

## C. Cohort evidence (one row per session; ≥ 20 required)

| session id | OS | created | `actual_kind` | `process_owner` | `authoritative` | `evidence_freshness` | duration | exit | notes |
|---|---|---|---|---|---|---|---|---|---|
| MISSING | | | | | | | | | |

Aggregate (observed):

| Metric | Budget | Observed |
|---|---|---|
| Sessions counted (verified direct) | ≥ 20 | MISSING |
| Soak, continuous | ≥ 24 h | MISSING |
| Error rate | < 1.0 % | MISSING |
| Fallback rate | < 5.0 % | MISSING |
| Keystroke echo p99 | ≤ 50 ms | MISSING |
| Attach → first byte p99 | ≤ 250 ms | MISSING |
| Resize → SIGWINCH p99 | ≤ 100 ms | MISSING |
| Adopt after recovery p99 | ≤ 2 s | MISSING |
| Lease takeover p99 | ≤ 200 ms | MISSING |
| RSS per session | ≤ 16 MiB | MISSING |
| FDs / goroutines per session | ≤ 6 / ≤ 6 | MISSING |
| Journal size | ≤ 64 MiB | MISSING |
| Survival loss | 0 % | MISSING |
| Duplicate processes / input bytes | 0 / 0 | MISSING |
| Stale-controller writes | 0 | MISSING |
| Secret leaks in durable stores | 0 | MISSING |
| Byte-exact visible parity | 100 % | MISSING |
| Replay order violations | 0 | MISSING |
| State accuracy / false approvals | ≥ 98 % / 0 | MISSING |

Workflow validation (TUI / CLI / native): attach · Ctrl-C/EOF/job control ·
fullscreen · resize · Cockpit pane · `wd send` · restore · web xterm · daemon
graceful restart · daemon SIGKILL (autopilot idle; record stop duration) —
each MISSING until observed.

## D. Expansion and drain

| Field | Value |
|---|---|
| Sign-off to make `runtime.pty: on` the default for new terminals (human, date) | MISSING |
| Post-expansion sample verified direct | MISSING |
| Legacy tmux plain-terminal count at start of drain | MISSING |
| Count at each check (date: n) | MISSING |
| Count reaches 0 by attrition (date) | MISSING |
| Any forced conversion/termination? (must be **No**) | MISSING |

## E. Rollback verification

| Field | Value |
|---|---|
| Rollback performed on live cohort (date) | MISSING |
| Existing direct sessions still controllable afterwards | MISSING |
| New sessions launched on tmux afterwards | MISSING |
| Downgrade rehearsal result (§5 procedure) | MISSING |

## F. Plan 03 handoff (`plan-5cd3c58b`)

Copy the checklist from the runbook §11 and mark each item with a link to the
filled evidence above. An unchecked or `MISSING` item blocks plan 03.
