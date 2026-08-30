package spend

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

// AntigravityParser approximates billed token usage from Antigravity's JSONL transcript.
type AntigravityParser struct{}

// ParseUsage approximates usage for Antigravity using byte length heuristics.
func (p *AntigravityParser) ParseUsage(r io.Reader) (u Usage, ok bool) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec antigravityRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		
		if rec.Type == "USER_INPUT" {
			u.InputTokens += len(rec.Content) / 4
			ok = true
		} else if rec.Type == "PLANNER_RESPONSE" {
			u.OutputTokens += (len(rec.Content) + len(rec.Thinking)) / 4
			ok = true
		}
	}
	
	return u, ok
}
