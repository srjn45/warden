// Package ptyevent is the provider-neutral contract for warden's event-driven
// PTY runtime (docs/specs/2026-10-19-pty-runtime-contract.md). It holds TYPES,
// ENUMS, VALIDATION and PURE DECISION FUNCTIONS only — no goroutines, no I/O, no
// process or tmux handling. The runtime (t2), the state/prompt classifiers (t3),
// the decision router (t4), the permission postures (t5) and the backend
// adapters (t6) all build on these definitions, so a behavior change here is a
// contract change: update the spec and the fixtures under testdata/ with it.
//
// The contract in one paragraph: a warden-owned PTY emits an append-only,
// per-session journal of Events with a monotonic Seq (gap-free) and a monotonic
// clock (MonoNS within an Epoch). Output is raw terminal bytes cut on UTF-8
// boundaries (SplitUTF8) with control sequences summarized (ControlScanner);
// everything persisted or fanned out beyond the live attach is redacted first
// (Redactor). Input carries an Origin and goes through input-ownership
// arbitration (Arbitrate) so Warden never types over a human. A pure reducer
// (Step) turns corroborated Evidence into the normalized State with
// debounce/hysteresis — never from silence alone. A normalized Prompt
// (fingerprint, capabilities, option semantics, scope, provenance, confidence)
// is routed by Decide(posture × classification) and every decision is
// revalidated against the live prompt (Revalidate) immediately before its
// keystrokes are written (TOCTOU).
package ptyevent
