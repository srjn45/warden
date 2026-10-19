// Package promptengine is the live seam that connects the event-driven state
// classifier (internal/stateclassifier) to the normalized prompt router
// (internal/decisionrouter) and to the session's input path — a warden-owned
// PTY or a mirrored tmux pane — for the adapters that implement the contract
// (docs/specs/2026-10-19-pty-runtime-contract.md §9–§11, §14 t6).
//
// The classifier reports a prompt on screen; the engine resolves the session's
// posture, runs the router (posture → guards → live revalidation → arbitrated
// input) and remembers the outcome so one prompt is answered once. A screen the
// parsers cannot read goes to Fast-Brain recognition and, failing a usable
// learned shape, escalates to needs_attention — it is never answered blind.
//
// In runtime.pty=shadow the engine runs the identical pipeline as a dry run:
// it journals what it would have done and writes nothing.
package promptengine
