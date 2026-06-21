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

// LimitBannerPresent reports whether pane's trailing lines show Claude's limit
// banner. It is exported for the daemon's resume gate so the gate reuses the
// exact detectRateLimit logic (trailing window + banner anchor) instead of
// duplicating the keyword list.
func LimitBannerPresent(pane string) bool {
	ok, _, _ := detectRateLimit(pane)
	return ok
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
	// (am|pm) is captured adjacent to the time so the am/pm decision has a single
	// source of truth — an unrelated "pm" elsewhere in the pane can't flip it.
	reClaudeCode := regexp.MustCompile(`(?i)resets\s+(\d{1,2}:\d{2})(am|pm)?\s*\(([^)]+)\)`)
	if m := reClaudeCode.FindStringSubmatch(pane); len(m) == 4 {
		timeStr, ampm, tzName := m[1], strings.ToLower(m[2]), m[3]

		loc, err := time.LoadLocation(tzName)
		if err != nil {
			return time.Time{}, false
		}

		layout := "15:04"
		if ampm != "" {
			layout, timeStr = "3:04pm", timeStr+ampm
		}
		resetTime, err := time.ParseInLocation(layout, timeStr, loc)
		if err != nil {
			return time.Time{}, false
		}

		now := time.Now().In(loc)
		result := time.Date(
			now.Year(), now.Month(), now.Day(),
			resetTime.Hour(), resetTime.Minute(), 0, 0, loc,
		)
		// A clock-time earlier than now is the NEXT occurrence, not one already
		// past — roll to tomorrow so we never schedule an immediate retry loop.
		if result.Before(now) {
			result = result.Add(24 * time.Hour)
		}
		return result, true
	}

	// Pattern 2: Generic zone-less "at HH:MM" patterns (fallback).
	reGeneric := regexp.MustCompile(`(?i)(?:at|again at)\s+(\d{1,2}:\d{2})\s*(am|pm)?`)
	if m := reGeneric.FindStringSubmatch(pane); len(m) >= 2 {
		timeStr, ampm := m[1], strings.ToLower(m[2])

		layout := "15:04"
		if ampm != "" {
			layout, timeStr = "3:04pm", timeStr+ampm
		}
		resetTime, err := time.Parse(layout, timeStr)
		if err != nil {
			return time.Time{}, false
		}

		now := time.Now()
		result := time.Date(
			now.Year(), now.Month(), now.Day(),
			resetTime.Hour(), resetTime.Minute(), 0, 0, now.Location(),
		)
		// Roll a past clock-time forward so the zone-less fallback never returns a
		// time before now; the scheduler's buffer absorbs any cross-zone skew.
		if result.Before(now) {
			result = result.Add(24 * time.Hour)
		}
		return result, true
	}

	return time.Time{}, false
}
