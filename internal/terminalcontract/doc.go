// Package terminalcontract is the machine-checkable half of
// docs/specs/2026-10-19-plain-terminal-inventory-and-contract.md (plan 02 t1).
//
// It holds DATA ONLY — the coverage matrix of plain-terminal paths, the gap
// register against the plan 01 PTY runtime, the rollout/parity/resource/
// latency/error budgets and the deterministic fixture matrix — plus tests that
// keep that data honest: every matrix row's code anchor must still exist, every
// ID must appear in the spec, and budgets must be internally consistent. A
// change to a terminal path, a closed gap or a budget is a change to this
// package AND to the spec, together (same rule as internal/ptyevent).
//
// Later plan 02 tasks (t2 lifecycle, t3 attach/recovery, t4 rollout, t5
// validation) consume these IDs: a fixture ID is the name of the test or
// benchmark that must exist when that task closes the gap that owns it.
package terminalcontract
