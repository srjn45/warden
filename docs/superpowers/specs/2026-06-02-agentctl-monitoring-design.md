# agentctl Monitoring + Notifications — Design

**Date:** 2026-06-02
**Status:** Approved design (pre-implementation)
**Owner:** Srajan Pathak (personal project)
**Sub-project 5 of 5** (1 storage ✅ → 2 session-id ✅ → 4 restore ✅ → 3 teardown ✅ → **5 monitoring/notify**).

---

## 1. Goal

Proactively notify the user when an agent needs attention — without watching the dashboard. The poller already *monitors* each agent (classifying `working`/`waiting_for_input`/`idle`/`orphaned`/`errored` and detecting stuck agents); this sub-project adds the **notification layer** that fires a macOS desktop notification on transitions into states that need the user.

## 2. Detection — already implemented (no work here)

`poller.tick` calls `SessionAlive` + `classify` each tick and swaps status via `UpdateStatusIf` only on a real change. The actionable states already exist: `waiting_for_input` (agent is asking), `idle` (stale `working` past `stuckAfter` → stuck), `orphaned` (tmux gone), `errored`. This sub-project hooks the **transition edge**, so detection is reused as-is.

## 3. `Notifier` interface — new `internal/notify` package

```go
type Notifier interface {
	Notify(title, body string)
}
```
- **`osaNotifier` (darwin):** execs `osascript -e 'display notification <body> with title <title>'`. Best-effort — logs on error, never blocks or panics. Takes an exec function (the existing `lifecycle.Runner`, or a minimal `func(name string, args ...string) error`) for testability.
- **`logNotifier`:** writes a single log line (`log.Printf`). Used on non-darwin OR when notifications are disabled.
- **`notify.New(enabled bool) Notifier`:** returns `osaNotifier` when `runtime.GOOS == "darwin"` && `enabled`, else `logNotifier`. Pluggable — a future Slack notifier slots in behind the same interface.

osascript values are passed as exec args (never interpolated into a shell), so subjects with quotes/newlines are safe.

## 4. Poller hook — `OnTransition`

Add a field to `Poller` (sibling of `OnChange`):
```go
// OnTransition, if set, is called once per successful status swap with the
// session and its old/new status. Edge-triggered, so callers are notified once
// per transition (never per tick).
OnTransition func(sess *store.Session, from, to store.Status)
```
In `tick`, immediately after the successful swap (`else if ok { changed = true }` at poller.go:124-125), call:
```go
if p.OnTransition != nil {
	p.OnTransition(s, s.Status, next)
}
```
(`s.Status` is the pre-swap snapshot value; `next` is the new one.) No call when `next == s.Status` or when the CAS doesn't take.

## 5. Daemon wiring

In the daemon's poller setup (where `OnChange` is wired, `server.go`):
- Construct the notifier once (passed in / built in `cli/daemon.go` from config) and set:
```go
p.OnTransition = func(sess *store.Session, _ , to store.Status) {
	title, body, ok := notifyMessage(sess, to)
	if !ok {
		return // non-actionable transition
	}
	go notifier.Notify(title, body) // best-effort, never blocks the poll loop
}
```
- `notifyMessage(sess, to)` returns `(title, body, actionable)`:
  | `to` | title | body |
  |---|---|---|
  | `waiting_for_input` | `agentctl — needs input` | `<id>: <subject>` |
  | `idle` | `agentctl — stuck` | `<id> went idle: <subject>` |
  | `orphaned` | `agentctl — agent lost` | `<id> tmux gone: <subject>` |
  | `errored` | `agentctl — errored` | `<id>: <subject>` |
  | anything else | — | — (`actionable=false`) |
  (Subject falls back to the id if empty.)

`cli/daemon.go` builds the notifier via `notify.New(cfg.NotifyEnabled)` and passes it to the server (new `NewServer` param or a setter).

## 6. Config

`internal/config`: add `NotifyEnabled bool` from `AGENTCTL_NOTIFY` (default **on**; `"0"`/`"off"`/`"false"` → disabled → `logNotifier`).

## 7. Testing

- **poller (`OnTransition`):** with a fake `Deps`, a session that transitions (e.g. `working`→`waiting_for_input`) invokes `OnTransition` once with the right (from,to); a tick with no status change does NOT invoke it. (Capture via a closure.)
- **daemon (`notifyMessage` + wiring):** `notifyMessage` returns `actionable=true` with the right title/body for each of the 4 states and `false` for `working`/`spawning`/`done`; subject-empty falls back to id. A fake `Notifier` captures that the wired `OnTransition` fires for actionable states only.
- **notify:** `osaNotifier.Notify` calls the exec func with `osascript -e <script>` where the script embeds title+body (assert the args via a fake exec). `New(true)` on darwin → `osaNotifier`; `New(false)` → `logNotifier` (assert by type or behavior).

## 8. Caveat (documented in README)

`osascript display notification` reaches Notification Center only when the daemon runs in the user's GUI login session (a terminal, or a launchd **user agent** — `gui/<uid>` domain). A headless/system daemon won't show desktop notifications; the `logNotifier` fallback (or disabling) covers that. The user runs the daemon in their session, so this works.

## 9. Out of scope

- Slack / other channels (the `Notifier` interface is ready; not built now).
- Notifying on non-actionable transitions (`working`/`spawning`/`done`).
- Re-notify / escalation / snooze / per-agent mute.
- A separate monitoring goroutine — the existing poller tick is the monitor; no new loop is added.
