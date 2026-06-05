package digest

import (
	"bufio"
	"encoding/json"
	"io"
)

// record is the minimal shape we read from each JSONL line. Most transcript
// records (last-prompt, attachment, system, file-history-snapshot, …) are not
// conversation turns and are skipped via the top-level Type discriminator.
type record struct {
	Type    string `json:"type"`
	Message struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"` // string OR []block
	} `json:"message"`
}

type block struct {
	Type  string          `json:"type"`  // "text" | "tool_use" | "tool_result"
	Text  string          `json:"text"`  // for type=="text"
	Name  string          `json:"name"`  // for type=="tool_use"
	Input json.RawMessage `json:"input"` // for type=="tool_use"
}

type toolInput struct {
	FilePath     string `json:"file_path"`
	NotebookPath string `json:"notebook_path"`
}

var editTools = map[string]bool{"Write": true, "Edit": true, "MultiEdit": true, "NotebookEdit": true}

// ParseTranscript reads a Claude Code transcript JSONL stream and returns
// deterministic Facts. Malformed lines are skipped (not fatal); only a reader
// error is returned.
func ParseTranscript(r io.Reader) (Facts, error) {
	var f Facts
	seen := map[string]bool{}

	sc := bufio.NewScanner(r)
	// Transcript lines (esp. tool_result payloads) can be very long; raise the
	// scanner's per-line cap well above the 64K default.
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec record
		if err := json.Unmarshal(line, &rec); err != nil {
			continue // malformed line — skip
		}
		switch rec.Type {
		case "assistant":
			f.Turns++
			text, files := assistantParts(rec.Message.Content)
			for _, p := range files {
				if p != "" && !seen[p] {
					seen[p] = true
					f.EditedFiles = append(f.EditedFiles, p)
				}
			}
			if text != "" {
				f.LastMessage = text // keep the last assistant text seen
			}
		case "user":
			if f.Task != "" {
				continue
			}
			if t := userPrompt(rec.Message.Content); t != "" {
				f.Task = t
			}
		}
	}
	if err := sc.Err(); err != nil {
		return f, err
	}
	return f, nil
}

// assistantParts returns the concatenated text and any edit-tool file targets in
// an assistant message. content is either a JSON string or a list of blocks.
func assistantParts(content json.RawMessage) (text string, files []string) {
	if s, ok := asString(content); ok {
		return s, nil
	}
	var blocks []block
	if err := json.Unmarshal(content, &blocks); err != nil {
		return "", nil
	}
	for _, b := range blocks {
		switch b.Type {
		case "text":
			text += b.Text
		case "tool_use":
			if editTools[b.Name] {
				var in toolInput
				_ = json.Unmarshal(b.Input, &in)
				p := in.FilePath
				if p == "" {
					p = in.NotebookPath
				}
				files = append(files, p)
			}
		}
	}
	return text, files
}

// userPrompt returns the prompt text of a user message, or "" if the record is
// only tool_result blocks (not an actual prompt).
func userPrompt(content json.RawMessage) string {
	if s, ok := asString(content); ok {
		return s
	}
	var blocks []block
	if err := json.Unmarshal(content, &blocks); err != nil {
		return ""
	}
	var text string
	for _, b := range blocks {
		if b.Type == "text" {
			text += b.Text
		}
	}
	return text
}

// asString reports whether raw is a JSON string and returns its value.
func asString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || raw[0] != '"' {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}
