package poller

import (
	"strings"
	"time"
)

// detectRateLimit checks if pane content indicates a rate limit hit.
// Returns (isLimited, restoreTime, ok) where:
//   - isLimited: true if rate limit detected
//   - restoreTime: parsed restore timestamp (zero if not found)
//   - ok: true if restoreTime was successfully parsed
func detectRateLimit(pane string) (bool, time.Time, bool) {
	// Pattern 1: Look for common rate limit keywords
	limitKeywords := []string{
		"rate limit",
		"usage limit",
		"session limit",
		"quota exceeded",
	}

	hasLimit := false
	paneLower := strings.ToLower(pane)
	for _, kw := range limitKeywords {
		if strings.Contains(paneLower, kw) {
			hasLimit = true
			break
		}
	}

	if !hasLimit {
		return false, time.Time{}, false
	}

	// Pattern 2: Try to parse restore time
	restoreTime, ok := parseRestoreTime(pane)
	return true, restoreTime, ok
}

// parseRestoreTime attempts to extract a restore timestamp from the error message.
// Returns (time, true) if successful, (zero, false) otherwise.
//
// NOTE: This is a placeholder implementation until the exact Claude Code error
// message format is known. Will be updated with regex patterns once the user
// provides the actual error message.
//
// Expected patterns to support (examples):
//
//	"Try again at 3:45 PM"
//	"Available again at 15:45"
//	"Reset at 2024-06-14 15:45:00"
//	"retry_after: 1718380800" (unix timestamp)
func parseRestoreTime(pane string) (time.Time, bool) {
	// Placeholder: always returns false until exact format is known
	// TODO: Add regex patterns when user provides actual error message
	return time.Time{}, false
}
