package spend

import "io"

// SpendParser extracts the cumulative billed token usage from a transcript.
type SpendParser interface {
	// ParseUsage scans a transcript stream and sums billed tokens across all turns.
	// ok=false if the transcript is empty or has no recorded usage yet.
	ParseUsage(r io.Reader) (u Usage, ok bool)
}

// GetParser returns the appropriate SpendParser for the given backend.
func GetParser(backend string) SpendParser {
	switch backend {
	case "antigravity":
		return &AntigravityParser{}
	default:
		return &ClaudeParser{}
	}
}
