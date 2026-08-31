// Package spend reads an agent's cumulative billed token usage from its
// transcript JSONL and persists a per-session running total. Where the
// savings ledger records the counterfactual (what warden kept OUT of context),
// the spend tracker records the REAL denominator: what an agent actually billed
// to the backend. Together they let warden frame a saving as a share of measured spend
// ("cut measured spend ~X%") rather than only against a counterfactual.
//
// The package is split pure/impure like internal/savings and internal/metrics:
// the transcript parser here is unit-testable with no I/O; store.go owns the
// per-session JSON ledger. Everything is best-effort and fail-open — a missing,
// rotated, or partially written transcript yields the best partial figure (or
// ok=false), never an error that could break the action being measured.
package spend

// Usage is the cumulative billed token usage parsed from a transcript. The
// precise accounting is backend-specific: Claude sums per-turn uncached input
// and output, while Codex reports cumulative snapshots including cached input
// and reasoning output.
type Usage struct {
	InputTokens  int
	OutputTokens int
}

// Total is the cumulative input+output tokens — the single figure the spend
// store persists per session and sums into the measured-spend denominator.
func (u Usage) Total() int { return u.InputTokens + u.OutputTokens }
