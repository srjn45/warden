package ctxtokens

import "io"

// TokenParser extracts the context-window token gauge from an agent's transcript.
type TokenParser interface {
	// LatestContextTokens scans a transcript stream and returns the latest token
	// count. ok=false if the transcript is empty or has no recorded usage yet.
	LatestContextTokens(r io.Reader) (tokens int, ok bool)
}

// GetParser returns the appropriate TokenParser for the given backend.
func GetParser(backend string) TokenParser {
	switch backend {
	case "antigravity":
		return &AntigravityParser{}
	default:
		return &ClaudeParser{}
	}
}
