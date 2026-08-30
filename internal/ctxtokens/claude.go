package ctxtokens

import (
	"bufio"
	"encoding/json"
	"io"
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

// ClaudeParser reads context-window occupancy from Claude Code's JSONL transcript.
type ClaudeParser struct{}

// LatestContextTokens scans a transcript JSONL stream and returns the context
// fill of the LAST reading: an assistant turn's usage block or a compaction boundary.
func (p *ClaudeParser) LatestContextTokens(r io.Reader) (tokens int, ok bool) {
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
