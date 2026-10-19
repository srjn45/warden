// Package stateclassifier implements the generic, provider-neutral,
// event-driven activity/quiescence classifier for warden sessions (t3 of the
// PTY runtime contract, docs/specs/2026-10-19-pty-runtime-contract.md §8 and §14).
//
// The classifier replaces poller-based heuristic status detection with an
// event-driven state machine:
//
//   - Active terminal events (output, input) reset the quiescence timer.
//   - Configurable quiescence grace windows trigger an investigation (gathering
//     corroborating evidence across screen models, process trees, transcripts,
//     and backend hooks) — never a state assertion from silence alone.
//   - Evidence is fused into ptyevent.Observation and folded via
//     ptyevent.Step(DefaultHysteresis).
//   - When a state transition commits, it is journaled via Stream.Emit as a
//     ptyevent.KindState event carrying its explainable evidence trail.
//   - In runtime.pty mode "on", the classifier is authoritative for all
//     runtime-backed sessions, updating store status while the legacy poller
//     skips pane capture and classification so it cannot fight the stream.
//   - In runtime.pty mode "shadow", the classifier observes mirrored events and
//     logs disagreements without mutating the store.
package stateclassifier
