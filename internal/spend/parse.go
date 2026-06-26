// Package spend reads an agent's cumulative billed token usage from its Claude
// Code transcript JSONL and persists a per-session running total. Where the
// savings ledger records the counterfactual (what warden kept OUT of context),
// the spend tracker records the REAL denominator: what an agent actually billed
// to Claude. Together they let warden frame a saving as a share of measured spend
// ("cut measured Claude spend ~X%") rather than only against a counterfactual.
//
// The package is split pure/impure like internal/savings and internal/metrics:
// the transcript parser here is unit-testable with no I/O; store.go owns the
// per-session JSON ledger. Everything is best-effort and fail-open — a missing,
// rotated, or partially written transcript yields the best partial figure (or
// ok=false), never an error that could break the action being measured.
package spend

import (
	"bufio"
	"encoding/json"
	"io"
)

// Usage is the cumulative billed token usage parsed from a transcript: the sum
// over every assistant turn of its per-turn input and output tokens. Input is
// the new (uncached) prompt tokens and Output the generated tokens — the two
// fields that bill at full rate. Cache read/creation tokens are deliberately
// excluded: they re-bill the same context warden is working to keep small on
// every turn, so summing them would inflate the denominator with re-reads rather
// than counting genuinely new spend (and warden's own compaction shrinks them).
type Usage struct {
	InputTokens  int
	OutputTokens int
}

// Total is the cumulative input+output tokens — the single figure the spend
// store persists per session and sums into the measured-spend denominator.
func (u Usage) Total() int { return u.InputTokens + u.OutputTokens }

type usageRecord struct {
	Type    string `json:"type"`
	Message struct {
		Usage *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

// ParseUsage scans a transcript JSONL stream and sums input_tokens+output_tokens
// across every assistant turn that carries a usage block. ok=false means no such
// turn was found (a just-spawned agent or an empty/missing transcript), in which
// case Usage is zero and callers should treat the spend as unknown rather than 0.
// Malformed lines are skipped (best-effort) — mirrors ctxtokens.LatestContextTokens
// and the savings ledger scanner, so a partial/rotated file still yields a usable
// partial total.
func ParseUsage(r io.Reader) (u Usage, ok bool) {
	sc := bufio.NewScanner(r)
	// Transcript lines (tool_result payloads) can be large; match the context
	// gauge's generous cap so a big line is parsed, not silently truncated.
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec usageRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		if rec.Type != "assistant" || rec.Message.Usage == nil {
			continue
		}
		u.InputTokens += rec.Message.Usage.InputTokens
		u.OutputTokens += rec.Message.Usage.OutputTokens
		ok = true
	}
	return u, ok
}
