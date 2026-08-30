package spend

import (
	"bufio"
	"encoding/json"
	"io"
)

type usageRecord struct {
	Type    string `json:"type"`
	Message struct {
		Usage *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

// ClaudeParser reads billed token usage from Claude Code's JSONL transcript.
type ClaudeParser struct{}

// ParseUsage scans a transcript JSONL stream and sums input_tokens+output_tokens
// across every assistant turn that carries a usage block.
func (p *ClaudeParser) ParseUsage(r io.Reader) (u Usage, ok bool) {
	sc := bufio.NewScanner(r)
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
