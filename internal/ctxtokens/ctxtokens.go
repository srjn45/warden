// Package ctxtokens reads an agent's current context-window occupancy from its
// Claude Code transcript JSONL and classifies it against warn/critical
// thresholds. The gauge is the most recent assistant turn's input + cached
// tokens — the same quantity /context reports, obtained passively (no keystroke
// injection, no TUI scraping) — or, when a compaction landed after that turn,
// the boundary's post-compact fill.
package ctxtokens

import (
	"bufio"
	"encoding/json"
	"io"
)

// State is an agent's context-fill band.
type State string

const (
	StateOK       State = "ok"
	StateWarning  State = "warning"
	StateCritical State = "critical"
)

type usageRecord struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	Message struct {
		Usage *struct {
			InputTokens              int `json:"input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
	CompactMetadata *struct {
		PostTokens int `json:"postTokens"`
	} `json:"compactMetadata"`
}

// LatestContextTokens scans a transcript JSONL stream and returns the context
// fill of the LAST reading: an assistant turn's usage block
// (input_tokens + cache_read_input_tokens + cache_creation_input_tokens) or a
// compaction boundary's post-compact fill. The boundary matters: a landed
// /compact writes {"type":"system","subtype":"compact_boundary"} with
// compactMetadata.postTokens but NO new assistant turn until the next prompt,
// so without it the gauge would stay stuck at the pre-compact (critical)
// level and warden would keep re-sending /compact to an already-compacted,
// unprompted agent.
// ok=false means no model turn has been recorded yet (a just-spawned agent),
// in which case tokens is 0 and callers should treat the gauge as unknown.
// Malformed lines are skipped (not fatal); only the scanner's own error band is
// silently treated as end-of-data, yielding the best partial result.
func LatestContextTokens(r io.Reader) (tokens int, ok bool) {
	sc := bufio.NewScanner(r)
	// Transcript lines (tool_result payloads) can be large; match digest's cap.
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
		// Keep overwriting; the last reading in the stream wins.
		if rec.Type == "system" && rec.Subtype == "compact_boundary" {
			// postTokens is what survived the compaction (0 when the metadata
			// is absent — still a reset, just an unmeasured one).
			tokens = 0
			if rec.CompactMetadata != nil {
				tokens = rec.CompactMetadata.PostTokens
			}
			ok = true
			continue
		}
		if rec.Type != "assistant" || rec.Message.Usage == nil {
			continue
		}
		u := rec.Message.Usage
		tokens = u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
		ok = true
	}
	return tokens, ok
}

// Classify maps a token count to a state. warn and crit are inclusive lower
// bounds: tokens >= crit is critical, tokens >= warn (but < crit) is warning.
func Classify(tokens, warn, crit int) State {
	switch {
	case tokens >= crit:
		return StateCritical
	case tokens >= warn:
		return StateWarning
	default:
		return StateOK
	}
}
