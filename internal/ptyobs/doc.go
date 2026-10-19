// Package ptyobs is the operator-facing observability layer of the PTY runtime
// (docs/specs/2026-10-19-pty-runtime-contract.md, t7): a redacted, append-only,
// queryable per-session decision trace, a bounded-label metrics registry for
// PTY state detection and prompt recognition, and the human-readable
// permission summary every client (TUI, Cockpit, REST, MCP, CLI) renders.
//
// Boundaries:
//
//   - The trace stores only what the normalized prompt already carries (it is
//     redacted at the PTY writer) and re-redacts every free-text field on the
//     way to disk. It never stores raw keystrokes: the selected input is
//     described ("option 1 + Enter"), not recorded.
//   - Metric labels come from closed vocabularies. A free-form value (a CLI
//     version, an adapter id, a reason) is normalised or collapsed to "other";
//     a target, command, path, question, option label, session id or error text
//     can never become a label.
package ptyobs
