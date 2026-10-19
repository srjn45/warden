// Package ptyruntime is the warden-owned PTY runtime core (t2-1 of the PTY
// runtime contract, docs/specs/2026-10-19-pty-runtime-contract.md). It is a
// library: nothing in session lifecycle, the pollers or the daemon uses it yet
// (that wiring is t2-2, behind `runtime.pty: off|shadow|on`).
//
// Pieces:
//
//   - Session hosts one native interactive command on a PTY it owns. A reader
//     goroutine fans raw (unredacted) bytes to live RawSubscriptions and hands
//     them to the single per-session writer, which runs the contract pipeline
//     SplitUTF8 → Redactor → ControlScanner, feeds a Screen model, assigns Seq
//     and appends to the Journal BEFORE fanning the event out to
//     Subscriptions. A slow subscriber never blocks the PTY: it is cut off with
//     ErrResync and re-subscribes from its cursor (§6).
//   - Journal is the durable, append-only, per-session event log (JSONL
//     segment files). Every appended event passes ptyevent.Validate and the
//     ValidateStream ordering rules; segments start at a snapshot marker so
//     retention trims whole segments; Open truncates a torn tail.
//   - Input goes through ptyevent.Arbitrate (human always wins; automatic
//     input defers to a typing human) and ptyevent.RedactInput with the PTY's
//     live termios (SecretInput), so typed secrets never reach the journal.
//   - Host adapts a set of Sessions to tmuxproc.Host so lifecycle code can be
//     pointed at the PTY runtime without knowing about it.
//
// Only Linux and macOS are supported (the release targets).
package ptyruntime
