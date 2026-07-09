package poller

import (
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/approval"
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
	`(?i)(rate limit|usage limit|session limit|weekly limit|quota exceeded)[\s\S]{0,80}?resets\s`,
)

// claudeSpendLimitRe matches Claude Code's monthly-spend-cap banner. Unlike the
// usage/session/weekly banners it carries NO reset time — a spend cap clears
// only at billing rollover or when the user raises it at claude.ai — so a match
// tells the scheduler to fall back to the (long) spend retry interval rather
// than parse a reset clock-time. It anchors on the banner's distinctive verb
// phrases ("hit your monthly spend limit" / "adjust your monthly spend limit"),
// not a bare "spend limit", so ordinary agent prose about spend caps does not
// match — the same fail-closed stance as claudeLimitBannerRe.
var claudeSpendLimitRe = regexp.MustCompile(
	`(?i)(hit your monthly spend limit|adjust your monthly spend limit)`,
)

// limitMenuWaitRe matches the "Stop and wait for limit to reset" choice on
// Claude's rate-limit menu — the safe option that parks the agent until the
// limit clears, as opposed to "Upgrade your plan". Case-insensitive and
// tolerant of an optional article ("the limit").
var limitMenuWaitRe = regexp.MustCompile(`(?i)wait\s+for\s+(the\s+)?limit\s+to\s+reset`)

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
	switch {
	case claudeLimitBannerRe.MatchString(tail):
		// Parse the restore time from the banner region only, so the time-parse
		// logic never keys on am/pm or clock-times elsewhere in the scrollback.
		restoreTime, ok := ParseRestoreTime(tail)
		return true, restoreTime, ok
	case claudeSpendLimitRe.MatchString(tail):
		// Monthly spend cap: the banner carries no reset time, so signal limited
		// with ok=false and let the scheduler apply the long spend fallback.
		return true, time.Time{}, false
	default:
		return false, time.Time{}, false
	}
}

// SpendLimitBannerPresent reports whether pane's trailing lines show Claude's
// monthly-spend-cap banner (as opposed to the resettable usage/session/weekly
// banner). The daemon's scheduler uses this to pick the long spend retry
// interval, since a spend cap carries no in-band reset time and will not clear
// for hours or days.
func SpendLimitBannerPresent(pane string) bool {
	return claudeSpendLimitRe.MatchString(lastLines(pane, limitBannerTailLines))
}

// LimitMenuSelection describes how to answer Claude's rate-limit choice menu.
// waitIdx is the 1-based index of the "Stop and wait for limit to reset" choice;
// highlighted reports whether that safe option is the one the ❯ cursor currently
// sits on (so a bare Enter confirms it); ok is false when the pane is not that
// menu. It reuses approval.Parse so it keys on a real numbered menu (a ❯ cursor
// plus a sequential 1..N option run), never a stray numbered list in agent
// prose, and returns the matched index rather than assuming position 1 so a
// reordered menu still selects the safe wait option.
func LimitMenuSelection(pane string) (waitIdx int, highlighted bool, ok bool) {
	a, parsed := approval.Parse(pane)
	if !parsed {
		return 0, false, false
	}
	for i, opt := range a.Options {
		if limitMenuWaitRe.MatchString(opt) {
			idx := i + 1
			return idx, a.SelectedIdx == idx, true
		}
	}
	return 0, false, false
}

// LimitMenuOption reports the 1-based option index of the "Stop and wait for
// limit to reset" choice on Claude's rate-limit menu, or ok=false when the pane
// is not that menu. It is the highlight-agnostic form of LimitMenuSelection.
func LimitMenuOption(pane string) (int, bool) {
	idx, _, ok := LimitMenuSelection(pane)
	return idx, ok
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
// Supported patterns (tried in this order — the more specific banner shapes win
// before the zone-less generic fallback):
//   - "resets 1:30pm (Europe/Madrid)" — Claude Code session-limit clock format
//   - "resets 13:30 (Europe/Madrid)" — 24-hour variant
//   - "resets Thursday at 9am" / "resets Thu 09:00 (Europe/Madrid)" — weekly
//     banner carrying a weekday (with or without a time / timezone)
//   - "resets Jul 14" / "resets July 14 at 3pm (UTC)" / "resets 14 Jul 2026" —
//     weekly banner carrying a calendar date
//   - "Try again at 3:45 PM" / "Available again at 15:45" — generic clock time
//
// Weekday/date parsing is deliberately tolerant: it reads whatever a weekly
// banner carries and, when a piece is missing (no time → midnight; no timezone →
// local), still returns a usable resume time. Anything it cannot parse falls
// through to (zero, false) so the scheduler keeps its polling fallback rather
// than resuming at a wrong moment.
func ParseRestoreTime(pane string) (time.Time, bool) {
	if t, ok := parseResetClockTime(pane); ok {
		return t, true
	}
	if t, ok := parseResetWeekday(pane); ok {
		return t, true
	}
	if t, ok := parseResetCalendarDate(pane); ok {
		return t, true
	}
	if t, ok := parseResetGenericTime(pane); ok {
		return t, true
	}
	return time.Time{}, false
}

// resetClockTimeRe matches "resets 1:30pm (Europe/Madrid)" / "resets 13:30
// (Europe/Madrid)". (am|pm) is captured adjacent to the time so the am/pm
// decision has a single source of truth — an unrelated "pm" elsewhere in the
// pane can't flip it.
var resetClockTimeRe = regexp.MustCompile(`(?i)resets\s+(\d{1,2}:\d{2})(am|pm)?\s*\(([^)]+)\)`)

func parseResetClockTime(pane string) (time.Time, bool) {
	m := resetClockTimeRe.FindStringSubmatch(pane)
	if len(m) != 4 {
		return time.Time{}, false
	}
	loc, err := time.LoadLocation(m[3])
	if err != nil {
		return time.Time{}, false
	}
	hour, min, ok := parseClock(m[1], m[2])
	if !ok {
		return time.Time{}, false
	}
	now := time.Now().In(loc)
	result := time.Date(now.Year(), now.Month(), now.Day(), hour, min, 0, 0, loc)
	// A clock-time earlier than now is the NEXT occurrence, not one already past —
	// roll to tomorrow so we never schedule an immediate retry loop.
	if result.Before(now) {
		result = result.Add(24 * time.Hour)
	}
	return result, true
}

// weekdays maps the lowercased 3+ letter weekday prefix to its time.Weekday.
var weekdays = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday,
	"wed": time.Wednesday, "thu": time.Thursday, "fri": time.Friday,
	"sat": time.Saturday,
}

// months maps the lowercased 3-letter month prefix to its time.Month.
var months = map[string]time.Month{
	"jan": time.January, "feb": time.February, "mar": time.March,
	"apr": time.April, "may": time.May, "jun": time.June,
	"jul": time.July, "aug": time.August, "sep": time.September,
	"oct": time.October, "nov": time.November, "dec": time.December,
}

// resetWeekdayRe matches a weekly banner naming a weekday, e.g. "resets Thursday
// at 9am (Europe/Madrid)" or "resets Thu 09:00". The weekday name, an optional
// "at HH:MM(am|pm)", and an optional trailing "(tz)" are each captured; only the
// weekday itself is required.
var resetWeekdayRe = regexp.MustCompile(
	`(?i)resets\s+(?:on\s+)?(sun|mon|tue|wed|thu|fri|sat)[a-z]*(?:\s+(?:at\s+)?(\d{1,2}(?::\d{2})?)\s*(am|pm)?)?(?:\s*\(([^)]+)\))?`,
)

func parseResetWeekday(pane string) (time.Time, bool) {
	m := resetWeekdayRe.FindStringSubmatch(pane)
	if len(m) != 5 {
		return time.Time{}, false
	}
	wd, known := weekdays[strings.ToLower(m[1])]
	if !known {
		return time.Time{}, false
	}
	hour, min := 0, 0
	if m[2] != "" {
		var ok bool
		if hour, min, ok = parseClock(m[2], m[3]); !ok {
			return time.Time{}, false
		}
	}
	loc := resetLocation(m[4])
	now := time.Now().In(loc)
	// Days until the next occurrence of the named weekday (0 = today).
	days := (int(wd) - int(now.Weekday()) + 7) % 7
	result := time.Date(now.Year(), now.Month(), now.Day(), hour, min, 0, 0, loc).AddDate(0, 0, days)
	// If that lands at or before now (the weekday is today but the time already
	// passed), the banner means next week — roll forward a full week.
	if !result.After(now) {
		result = result.AddDate(0, 0, 7)
	}
	return result, true
}

// resetCalendarDateRe matches a weekly banner naming a calendar date in either
// "Month Day" ("resets Jul 14") or "Day Month" ("resets 14 Jul") order, with an
// optional year, optional "at HH:MM(am|pm)", and optional trailing "(tz)".
var resetCalendarDateRe = regexp.MustCompile(
	`(?i)resets\s+(?:on\s+)?(?:(jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)[a-z]*\s+(\d{1,2})|(\d{1,2})\s+(jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)[a-z]*)(?:,?\s+(\d{4}))?(?:\s+(?:at\s+)?(\d{1,2}(?::\d{2})?)\s*(am|pm)?)?(?:\s*\(([^)]+)\))?`,
)

func parseResetCalendarDate(pane string) (time.Time, bool) {
	m := resetCalendarDateRe.FindStringSubmatch(pane)
	if len(m) != 9 {
		return time.Time{}, false
	}
	// Either the "Month Day" branch (m[1],m[2]) or the "Day Month" branch
	// (m[3],m[4]) matched.
	var monName, dayStr string
	switch {
	case m[1] != "":
		monName, dayStr = m[1], m[2]
	case m[4] != "":
		monName, dayStr = m[4], m[3]
	default:
		return time.Time{}, false
	}
	mon, known := months[strings.ToLower(monName)]
	if !known {
		return time.Time{}, false
	}
	day, err := strconv.Atoi(dayStr)
	if err != nil || day < 1 || day > 31 {
		return time.Time{}, false
	}
	hour, min := 0, 0
	if m[6] != "" {
		var ok bool
		if hour, min, ok = parseClock(m[6], m[7]); !ok {
			return time.Time{}, false
		}
	}
	loc := resetLocation(m[8])
	now := time.Now().In(loc)
	year := now.Year()
	haveYear := false
	if m[5] != "" {
		if y, err := strconv.Atoi(m[5]); err == nil {
			year, haveYear = y, true
		}
	}
	result := time.Date(year, mon, day, hour, min, 0, 0, loc)
	// A year-less date that already passed this year means next year — roll it
	// forward so the resume never schedules into the past.
	if !haveYear && result.Before(now) {
		result = result.AddDate(1, 0, 0)
	}
	return result, true
}

// resetGenericTimeRe matches a generic zone-less "(again) at HH:MM(am|pm)".
var resetGenericTimeRe = regexp.MustCompile(`(?i)(?:at|again at)\s+(\d{1,2}:\d{2})\s*(am|pm)?`)

func parseResetGenericTime(pane string) (time.Time, bool) {
	m := resetGenericTimeRe.FindStringSubmatch(pane)
	if len(m) < 2 {
		return time.Time{}, false
	}
	hour, min, ok := parseClock(m[1], m[2])
	if !ok {
		return time.Time{}, false
	}
	now := time.Now()
	result := time.Date(now.Year(), now.Month(), now.Day(), hour, min, 0, 0, now.Location())
	// Roll a past clock-time forward so the zone-less fallback never returns a
	// time before now; the scheduler's buffer absorbs any cross-zone skew.
	if result.Before(now) {
		result = result.Add(24 * time.Hour)
	}
	return result, true
}

// parseClock turns a clock string plus an optional "am"/"pm" suffix into 24-hour
// hour/minute values. It accepts both "H:MM" and a bare "H" (minutes default to
// 0), with or without am/pm — so "9am", "9:30am", "14:30" and "14" all parse.
// ampm is matched case-insensitively; an empty ampm means a 24-hour clock.
func parseClock(clock, ampm string) (hour, min int, ok bool) {
	lower := strings.ToLower(ampm)
	hasMin := strings.Contains(clock, ":")
	var layout string
	switch {
	case lower != "" && hasMin:
		layout = "3:04pm"
	case lower != "":
		layout = "3pm"
	case hasMin:
		layout = "15:04"
	default:
		layout = "15"
	}
	timeStr := clock
	if lower != "" {
		timeStr += lower
	}
	t, err := time.Parse(layout, timeStr)
	if err != nil {
		return 0, 0, false
	}
	return t.Hour(), t.Minute(), true
}

// resetLocation resolves a captured timezone name to a *time.Location, falling
// back to the local zone when the banner carried no zone or an unknown one.
func resetLocation(tzName string) *time.Location {
	if tzName == "" {
		return time.Local
	}
	if loc, err := time.LoadLocation(tzName); err == nil {
		return loc
	}
	return time.Local
}
