package spend

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
}

// CodexParser reads billed token usage from Codex rollout JSONL events.
type CodexParser struct{}

// ParseUsage returns the last token_count snapshot rather than summing events:
// Codex token_count records are cumulative. input_tokens and output_tokens
// already include their cached-input and reasoning-output detail subsets.
func (p *CodexParser) ParseUsage(r io.Reader) (u Usage, ok bool) {
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

		tokens := rec.Payload.Info.TotalTokenUsage
		u.InputTokens = tokens.InputTokens
		u.OutputTokens = tokens.OutputTokens
		ok = true
	}
	return u, ok
}
