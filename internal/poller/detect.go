package poller

import (
	"regexp"
	"strings"
	"time"
)

// claudeLimitBannerRe matches Claude Code's limit banner. It requires a limit
// phrase together with the reset clause ParseRestoreTime keys on, so an agent
// merely printing "rate limit" (e.g. while writing or reviewing code) does not
// match — only the banner's distinctive "<limit phrase> … resets …" shape does.
//
// TODO(open-question): confirm the exact banner wording against a LIVE limit
// hit before relying on this in production. Until confirmed this errs toward
// failing CLOSED (a too-strict pattern misses a real limit) rather than open
// (misclassifying a working agent). All banner-dependent literals live here and
// in sampleLimitBanner (test fixture) so a correction lands in one place.
var claudeLimitBannerRe = regexp.MustCompile(
	`(?i)(rate limit|usage limit|session limit|quota exceeded)[\s\S]{0,80}?resets\s`,
)

// limitBannerTailLines is how many trailing pane lines we inspect for the
// banner. A real limit banner is the terminal state of the pane; anything that
// scrolled above it is stale output, not a live limit.
const limitBannerTailLines = 6

// detectRateLimit checks if pane content indicates a rate limit hit.
// Returns (isLimited, restoreTime, ok) where:
//   - isLimited: true if rate limit detected
//   - restoreTime: parsed restore timestamp (zero if not found)
//   - ok: true if restoreTime was successfully parsed
//
// It anchors on the limit banner in only the trailing lines (reusing lastLines)
// so neither stray "rate limit" text in agent output nor a banner that has
// since scrolled away is misread as a live limit.
func detectRateLimit(pane string) (bool, time.Time, bool) {
	tail := lastLines(pane, limitBannerTailLines)
	if !claudeLimitBannerRe.MatchString(tail) {
		return false, time.Time{}, false
	}
	// Parse the restore time from the banner region only, so the time-parse
	// logic never keys on am/pm or clock-times elsewhere in the scrollback.
	restoreTime, ok := ParseRestoreTime(tail)
	return true, restoreTime, ok
}

// ParseRestoreTime attempts to extract a restore timestamp from the error message.
// Returns (time, true) if successful, (zero, false) otherwise.
//
// Supported patterns:
//   - "resets 1:30pm (Europe/Madrid)" - Claude Code session limit format
//   - "resets 13:30 (Europe/Madrid)" - 24-hour variant
//   - "Try again at 3:45 PM" - generic format
//   - "Available again at 15:45" - 24-hour generic
func ParseRestoreTime(pane string) (time.Time, bool) {
	// Pattern 1: "resets 1:30pm (Europe/Madrid)" or "resets 13:30 (Europe/Madrid)"
	// Capture time (e.g., "1:30pm" or "13:30") and timezone (e.g., "Europe/Madrid")
	reClaudeCode := regexp.MustCompile(`resets\s+(\d{1,2}:\d{2})(?:am|pm)?\s*\(([^)]+)\)`)
	if matches := reClaudeCode.FindStringSubmatch(pane); len(matches) == 3 {
		timeStr := matches[1] // e.g., "1:30" or "13:30"
		tzName := matches[2]  // e.g., "Europe/Madrid"

		// Load the timezone
		loc, err := time.LoadLocation(tzName)
		if err != nil {
			return time.Time{}, false
		}

		// Determine if it's 12-hour or 24-hour format
		var layout string
		lowerPane := strings.ToLower(pane)
		if strings.Contains(lowerPane, "pm") {
			layout = "3:04pm"
		} else if strings.Contains(lowerPane, "am") {
			layout = "3:04am"
		} else {
			layout = "15:04"
		}

		// Parse time string with timezone
		now := time.Now().In(loc)
		resetTime, err := time.ParseInLocation(layout, timeStr+detectAmPm(pane, timeStr), loc)
		if err != nil {
			return time.Time{}, false
		}

		// Combine with today's date in the target timezone
		result := time.Date(
			now.Year(), now.Month(), now.Day(),
			resetTime.Hour(), resetTime.Minute(), 0, 0, loc,
		)

		// If the time is in the past, the rate limit should have already reset
		// Return current time to trigger immediate resume attempt
		if result.Before(now) {
			return now, true
		}

		return result, true
	}

	// Pattern 2: Generic "at HH:MM" patterns (fallback)
	reGeneric := regexp.MustCompile(`(?:at|again at)\s+(\d{1,2}:\d{2})\s*(am|pm|AM|PM)?`)
	if matches := reGeneric.FindStringSubmatch(pane); len(matches) >= 2 {
		timeStr := matches[1]
		ampm := ""
		if len(matches) >= 3 {
			ampm = strings.ToLower(matches[2])
		}

		layout := "15:04"
		if ampm == "am" || ampm == "pm" {
			layout = "3:04pm"
			timeStr = timeStr + ampm
		}

		now := time.Now()
		resetTime, err := time.Parse(layout, timeStr)
		if err != nil {
			return time.Time{}, false
		}

		result := time.Date(
			now.Year(), now.Month(), now.Day(),
			resetTime.Hour(), resetTime.Minute(), 0, 0, now.Location(),
		)

		// If the time is in the past, the rate limit should have already reset
		// Return current time to trigger immediate resume attempt
		if result.Before(now) {
			return now, true
		}

		return result, true
	}

	return time.Time{}, false
}

// detectAmPm extracts "am" or "pm" suffix from the pane text near the time string.
// Returns "am", "pm", or "" if not found.
func detectAmPm(pane, timeStr string) string {
	// Find the time string position and look for am/pm immediately after
	idx := strings.Index(strings.ToLower(pane), strings.ToLower(timeStr))
	if idx == -1 {
		return ""
	}

	// Look at the next 10 characters after the time
	remaining := pane[idx+len(timeStr):]
	if len(remaining) > 10 {
		remaining = remaining[:10]
	}

	lowerRemaining := strings.ToLower(remaining)
	if strings.HasPrefix(lowerRemaining, "pm") {
		return "pm"
	}
	if strings.HasPrefix(lowerRemaining, "am") {
		return "am"
	}

	return ""
}
