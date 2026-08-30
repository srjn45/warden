package ctxtokens

import (
	"bufio"
	"encoding/json"
	"io"
)

// antigravityRecord represents a log entry in Antigravity's transcript.jsonl.
type antigravityRecord struct {
	Type     string `json:"type"`
	Content  string `json:"content"`
	Thinking string `json:"thinking"`
}

// AntigravityParser approximates context-window occupancy from Antigravity's JSONL transcript.
type AntigravityParser struct{}

// LatestContextTokens scans an Antigravity transcript and estimates the token
// count using a heuristic (byte length / 4) since exact usage isn't emitted yet.
// Antigravity doesn't reset its transcript in the same way, so we just sum up the sizes
// of the interactions (USER_INPUT and PLANNER_RESPONSE).
func (p *AntigravityParser) LatestContextTokens(r io.Reader) (tokens int, ok bool) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	totalBytes := 0
	foundModelTurn := false

	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec antigravityRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}

		if rec.Type == "USER_INPUT" || rec.Type == "PLANNER_RESPONSE" {
			totalBytes += len(rec.Content) + len(rec.Thinking)
			foundModelTurn = true
		}
	}

	// Safe heuristic: 1 token ≈ 4 bytes
	if foundModelTurn {
		return totalBytes / 4, true
	}

	return 0, false
}
