package spend

import "io"

// SpendParser extracts cumulative billed token usage from a transcript.
type SpendParser interface {
	// ParseUsage scans a transcript stream for its cumulative billed token usage.
	// ok=false if the transcript is empty or has no recorded usage yet.
	ParseUsage(r io.Reader) (u Usage, ok bool)
}

// GetParser returns the appropriate SpendParser for the given backend.
func GetParser(backend string) SpendParser {
	switch backend {
	case "antigravity":
		return &AntigravityParser{}
	case "codex":
		return &CodexParser{}
	default:
		return &ClaudeParser{}
	}
}
