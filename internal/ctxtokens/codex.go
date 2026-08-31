package ctxtokens

import (
	"bufio"
	"encoding/json"
	"io"
)

// codexRecord represents an event from a Codex rollout JSONL stream.
type codexRecord struct {
	Type    string `json:"type"`
	Payload *struct {
		Type string `json:"type"`
		Info *struct {
			TotalTokenUsage *codexTokenUsage `json:"total_token_usage"`
		} `json:"info"`
	} `json:"payload"`
}

type codexTokenUsage struct {
	InputTokens           int `json:"input_tokens"`
	CachedInputTokens     int `json:"cached_input_tokens"`
	OutputTokens          int `json:"output_tokens"`
	ReasoningOutputTokens int `json:"reasoning_output_tokens"`
	TotalTokens           int `json:"total_tokens"`
}

// CodexParser reads context-window occupancy from Codex rollout JSONL events.
type CodexParser struct{}

// LatestContextTokens returns the total from the last token_count event. Codex
// emits cumulative snapshots, so the last snapshot is the current gauge.
func (p *CodexParser) LatestContextTokens(r io.Reader) (tokens int, ok bool) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec codexRecord
		if err := json.Unmarshal(line, &rec); err != nil || rec.Type != "event_msg" || rec.Payload == nil || rec.Payload.Type != "token_count" || rec.Payload.Info == nil || rec.Payload.Info.TotalTokenUsage == nil {
			continue
		}

		u := rec.Payload.Info.TotalTokenUsage
		tokens = u.TotalTokens
		if tokens == 0 && (u.InputTokens != 0 || u.CachedInputTokens != 0 || u.OutputTokens != 0 || u.ReasoningOutputTokens != 0) {
			tokens = u.InputTokens + u.CachedInputTokens + u.OutputTokens + u.ReasoningOutputTokens
		}
		ok = true
	}
	return tokens, ok
}
