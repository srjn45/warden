---
title: PTY runtime (event journal)
description: Turn on warden's own PTY runtime — a redacted, replayable per-session event journal streamed over a WebSocket — in shadow mode alongside tmux, or host plain-shell terminals directly.
---

warden has historically supervised agents by polling tmux: capture the pane, classify
the text, type answers back. The **PTY runtime** is the replacement foundation: an
ordered, **redacted**, replayable event journal per session, with a reconnectable
WebSocket stream. It is rolled out behind one setting, and the default changes nothing.

```yaml
# ~/.warden/config.yaml
runtime:
  pty: off      # off | shadow | on
```

The setting hot-reloads; a change applies to sessions created **afterwards**.

## Modes

| Mode | Agents | Plain-shell terminals | Journal / stream |
|---|---|---|---|
| `off` (default) | tmux, exactly as before | tmux, exactly as before | none |
| `shadow` | tmux, same launch path; output mirrored with `tmux pipe-pane` | tmux, mirrored | yes, observe-only |
| `on` | as `shadow` | hosted **directly** on a warden-owned PTY (no tmux session) | yes; interactive for terminals |

- **Shadow never writes to a pane.** The pollers keep deciding status and answering
  approvals exactly as today; the journal is a side channel.
- **Turning it back to `off`** stops every mirror (the panes keep running). Direct
  terminals keep running until they exit.
- **Restart:** tmux panes survive a daemon restart, and the next daemon re-adopts their
  mirrors on a new epoch (`lifecycle{recovered}`, a reset marker and a snapshot of the
  live screen). A direct terminal's shell is owned by the daemon and ends with it; the
  next daemon (in `on` mode) starts a fresh shell for every running direct terminal on
  the same journal, so a reconnecting client is resynchronized from the snapshot.

## What is in the journal

Every event (`output`, `input`, `resize`, `lifecycle`, `marker`, and later `state`,
`prompt`, `decision`) carries a session-wide `seq`, an `epoch` (a new process or a
re-adoption), and is **redacted before it is written**: API keys, tokens, bearer
headers, URL credentials, private keys and typed secrets become `[REDACTED:<kind>]`.
Journals live under `<data_dir>/pty/` (`direct/<id>` and `shadow/<id>`, `0700`/`0600`)
with generous retention (64 MiB per session, snapshots every 256 KiB of output).

Only the live, interactive attach of a direct terminal ever sees unredacted bytes: that
is the user's own terminal. Everything replayed is redacted.

## Streaming a session

`GET /api/v1/sessions/{id}/pty` upgrades to a WebSocket (same bearer token as the rest
of the API; `409` when the session has no journal). The client speaks first:

```json
{"type":"hello","mode":"observe","cursor":{"session_id":"A-1","epoch":1,"seq":42}}
```

The server answers with a `plan` (`tail`, `replay` or `snapshot`, plus `head`, the
replay/live boundary). Its additive `fidelity` is `redacted-text` for journal replay
or snapshots and `exact-live` for a live tail; older clients safely ignore that field.
It then sends one `{"type":"event","event":{…}}` frame per journaled
event in `seq` order — the replay from your cursor, then live. Keep the last `seq` you
saw: reconnecting with it resumes exactly where you left off. `{"type":"ack","seq":N}`
stores that checkpoint server-side for your `client_id`, so a later `hello` without a
cursor resumes from it.

- `reject` — the cursor cannot be served (another session, ahead of the journal):
  discard state and send a fresh `hello`.
- `resync` — you fell behind; send `hello` again with your last cursor.
- `snapshot` plans start with a snapshot marker: reset your screen to its text.

**Interactive** (`"mode":"interactive"`, direct terminals only, full token): binary
frames you send are keystrokes (journaled as human input, which always wins over
automatic input); `{"type":"resize","cols":120,"rows":40}` resizes the PTY. You
additionally receive the live, unredacted bytes as binary frames: render output events
with `seq <= head` from the event data and everything after from the binary frames.

Shadow (tmux-mirrored) sessions and **read-only tokens** are always observe-only.

## Prompt engine and permission postures

With `runtime.pty: on`, prompts read from the stream are normalized (capability,
target, option meaning, cancel path), re-validated against the live screen right
before any key is typed, and answered according to the session's permission posture.
A stale, changed, ambiguous or unrecognised prompt is never answered; it surfaces as
needs-attention. Strategic questions are never treated as permissions. In `shadow`
the engine only journals what it would have done.

Antigravity (`agy`) is integrated first: `dangerously-execute-all` uses its native
bypass mode and still answers the remaining permission and workspace-trust prompts;
`read-only` and `read-write` map to `plan` and `accept-edits`. Postures without an
exact native mode (such as `allowlisted`) are reported as degraded and enforced by
the router from the prompt stream. Formats warden has not seen are classified by
Fast-Brain on a redacted screen and only become learned shapes after deterministic
fixture replay, with an expiry and confidence floor.

## Observing the runtime

Every client renders the same documents:

```sh
warden inspect pty runtime <id>      # state, typed waiting reason, evidence, input owner, route, posture
warden inspect pty decisions <id>    # redacted, append-only decision trace (newest first)
warden inspect pty metrics           # warden_pty_* snapshot (Prometheus text at /api/v1/pty/metrics.prom)
```

The TUI info pane, the web Agent tab (runtime card + decision table) and the approvals
views show the same data, plus a human-readable summary of the pending permission and
its decision route. A session on `dangerously-execute-all` carries a conspicuous red
**DANGER** banner everywhere; the numbered answer buttons stay available.

The trace is bounded by `runtime.trace_max_records` (default 5000) and
`runtime.trace_max_age_days` (default 30), stores a description of the chosen input and
never raw bytes or secrets, and a write failure never blocks a decision (it increments
`warden_pty_decision_trace_errors_total`). Metric labels are closed vocabularies - no
prompt text, paths or secrets. Strategic questions (plan or design choices) are never
auto-answered in any posture; only permission and workspace-trust prompts are.
Alert rules and a Grafana dashboard live in
[`deploy/observability/`](https://github.com/srjn45/warden/tree/main/deploy/observability);
the full reference (migration, learned-shape safeguards, recovery) is
[`docs/PTY_OBSERVABILITY.md`](https://github.com/srjn45/warden/blob/main/docs/PTY_OBSERVABILITY.md).

## Survival: the PTY holder

With `runtime.pty: on`, each plain terminal runs under its own small **holder** process
(`warden ptyholder`, started in its own session). The holder — not the daemon — owns the
PTY master and the shell, so **restarting, updating or crashing the daemon does not touch
your shells**: the next daemon authenticates to the holder (private `0600` socket, peer uid,
a per-session credential, the recorded pid and start time of both holder and shell),
gets the master back and carries on with the same process, same working directory, same
scrollback and a new journal epoch (`lifecycle{recovered}`). It never relaunches a live
shell. If the holder is gone, the terminal is shown as `orphaned` with a journal entry that
names the cause and the next step (`wd agent restore <id>` starts a fresh shell) —
reconnecting is never silently replaced by a new shell. `WARDEN_PTY_HOLDER=off` keeps shells
as children of the daemon (they then die with it, as before).

Agents stay in tmux; the tmux server is their holder, and a restarted daemon re-mirrors a
pane only if its process (pid + start time) is the one it recorded.

Project close/reopen is a different thing: it deliberately ends the shell and starts a fresh
one (identity kept). It is never used for a daemon restart.

## Attaching: `wd attach`, the cockpit, controller lease

`wd attach <id>` attaches your terminal to a direct terminal natively: the local tty goes
raw (Ctrl-C, Ctrl-D, Ctrl-Z, full-screen programs, UTF-8/ANSI all behave as in a local
shell — signals and job control are the shell's PTY's), output is replayed then live
without gaps or repeats, window resizes follow your terminal, and the terminal is restored
on every way out. **Ctrl-]** then **.** detaches (the shell keeps running), **Ctrl-]** then
**t** takes control back, **Ctrl-] Ctrl-]** sends a literal Ctrl-]. `WARDEN_ATTACH=observe` watches
read-only. Tmux-hosted sessions still attach with tmux.

A session has any number of viewers and one **controller**. Attaching takes control
(human precedence) and demotes the previous controller, which keeps watching; its
keystrokes and resizes are rejected, never delivered. Input is tagged with the lease
generation and a per-client id, so a flaky connection that resends a command does not run
it twice — also across a daemon restart. Raw bytes shown to you are the program's own
(faithful); the journal, snapshots, audit and anything remote-safe are redacted, and typed
secrets are journaled as a length only.

## Compatibility

- `GET /api/v1/sessions/{id}/attach` keeps working for every session. For a direct
  terminal it bridges the same raw protocol to the runtime (current screen first, then
  live bytes), so the web UI and the Android app need no change. This legacy bridge is
  intentionally frozen; new attach/control clients use `/pty`.
- `wd send`/session input and output also route to a direct terminal.
- A direct terminal has **no tmux session**, so `tmux attach` does not apply to it. Use
  `wd attach <id>` (below) — the cockpit's terminal pane does exactly that — or the
  daemon's `/attach` / `/pty` endpoints. Use `shadow` if you prefer tmux for terminals.
- The design and the event contract are in
  [`docs/specs/2026-10-19-pty-runtime-contract.md`](https://github.com/srjn45/warden/blob/main/docs/specs/2026-10-19-pty-runtime-contract.md).
