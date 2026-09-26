package backends

// Rate-limit parse helpers shared across backend adapters.
//
// These functions mirror poller/detect.go's ParseRestoreTime family but live
// here to avoid an import cycle: poller_test.go (package poller) imports
// backends, so backends cannot import poller.

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// limitLastLines returns the last n non-empty trailing lines of s. It mirrors
// poller.lastLines and is used by all DetectRateLimit implementations to anchor
// matches to the live bottom of the pane rather than scrollback.
func limitLastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// parseRateLimitResetTime mirrors poller.ParseRestoreTime: it extracts a reset
// time from pane text, trying each supported pattern in order. Returns
// (time, true) on success, (zero, false) when nothing matched.
func parseRateLimitResetTime(pane string) (time.Time, bool) {
	if t, ok := rlParseClockTime(pane); ok {
		return t, true
	}
	if t, ok := rlParseWeekday(pane); ok {
		return t, true
	}
	if t, ok := rlParseCalendarDate(pane); ok {
		return t, true
	}
	if t, ok := rlParseGenericTime(pane); ok {
		return t, true
	}
	return time.Time{}, false
}

// rlResetClockTimeRe matches "resets 1:30pm (Europe/Madrid)" / "resets 13:30 (Europe/Madrid)".
var rlResetClockTimeRe = regexp.MustCompile(`(?i)resets\s+(\d{1,2}:\d{2})(am|pm)?\s*\(([^)]+)\)`)

func rlParseClockTime(pane string) (time.Time, bool) {
	m := rlResetClockTimeRe.FindStringSubmatch(pane)
	if len(m) != 4 {
		return time.Time{}, false
	}
	loc, err := time.LoadLocation(m[3])
	if err != nil {
		return time.Time{}, false
	}
	hour, min, ok := rlParseClock(m[1], m[2])
	if !ok {
		return time.Time{}, false
	}
	now := time.Now().In(loc)
	result := time.Date(now.Year(), now.Month(), now.Day(), hour, min, 0, 0, loc)
	if result.Before(now) {
		result = result.Add(24 * time.Hour)
	}
	return result, true
}

var rlWeekdays = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday,
	"wed": time.Wednesday, "thu": time.Thursday, "fri": time.Friday,
	"sat": time.Saturday,
}

var rlMonths = map[string]time.Month{
	"jan": time.January, "feb": time.February, "mar": time.March,
	"apr": time.April, "may": time.May, "jun": time.June,
	"jul": time.July, "aug": time.August, "sep": time.September,
	"oct": time.October, "nov": time.November, "dec": time.December,
}

var rlResetWeekdayRe = regexp.MustCompile(
	`(?i)resets\s+(?:on\s+)?(sun|mon|tue|wed|thu|fri|sat)[a-z]*(?:\s+(?:at\s+)?(\d{1,2}(?::\d{2})?)\s*(am|pm)?)?(?:\s*\(([^)]+)\))?`,
)

func rlParseWeekday(pane string) (time.Time, bool) {
	m := rlResetWeekdayRe.FindStringSubmatch(pane)
	if len(m) != 5 {
		return time.Time{}, false
	}
	wd, known := rlWeekdays[strings.ToLower(m[1])]
	if !known {
		return time.Time{}, false
	}
	hour, min := 0, 0
	if m[2] != "" {
		var ok bool
		if hour, min, ok = rlParseClock(m[2], m[3]); !ok {
			return time.Time{}, false
		}
	}
	loc := rlResetLocation(m[4])
	now := time.Now().In(loc)
	days := (int(wd) - int(now.Weekday()) + 7) % 7
	result := time.Date(now.Year(), now.Month(), now.Day(), hour, min, 0, 0, loc).AddDate(0, 0, days)
	if !result.After(now) {
		result = result.AddDate(0, 0, 7)
	}
	return result, true
}

var rlResetCalendarDateRe = regexp.MustCompile(
	`(?i)resets\s+(?:on\s+)?(?:(jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)[a-z]*\s+(\d{1,2})|(\d{1,2})\s+(jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)[a-z]*)(?:,?\s+(\d{4}))?(?:\s+(?:at\s+)?(\d{1,2}(?::\d{2})?)\s*(am|pm)?)?(?:\s*\(([^)]+)\))?`,
)

func rlParseCalendarDate(pane string) (time.Time, bool) {
	m := rlResetCalendarDateRe.FindStringSubmatch(pane)
	if len(m) != 9 {
		return time.Time{}, false
	}
	var monName, dayStr string
	switch {
	case m[1] != "":
		monName, dayStr = m[1], m[2]
	case m[4] != "":
		monName, dayStr = m[4], m[3]
	default:
		return time.Time{}, false
	}
	mon, known := rlMonths[strings.ToLower(monName)]
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
		if hour, min, ok = rlParseClock(m[6], m[7]); !ok {
			return time.Time{}, false
		}
	}
	loc := rlResetLocation(m[8])
	now := time.Now().In(loc)
	year := now.Year()
	haveYear := false
	if m[5] != "" {
		if y, err := strconv.Atoi(m[5]); err == nil {
			year, haveYear = y, true
		}
	}
	result := time.Date(year, mon, day, hour, min, 0, 0, loc)
	if !haveYear && result.Before(now) {
		result = result.AddDate(1, 0, 0)
	}
	return result, true
}

// rlResetGenericTimeRe matches a generic "(again) at HH:MM(am|pm)" or
// "available (again) at HH:MM(am|pm)".
var rlResetGenericTimeRe = regexp.MustCompile(`(?i)(?:at|again at|available at|available again at)\s+(\d{1,2}:\d{2})\s*(am|pm)?`)

func rlParseGenericTime(pane string) (time.Time, bool) {
	m := rlResetGenericTimeRe.FindStringSubmatch(pane)
	if len(m) < 2 {
		return time.Time{}, false
	}
	hour, min, ok := rlParseClock(m[1], m[2])
	if !ok {
		return time.Time{}, false
	}
	now := time.Now()
	result := time.Date(now.Year(), now.Month(), now.Day(), hour, min, 0, 0, now.Location())
	if result.Before(now) {
		result = result.Add(24 * time.Hour)
	}
	return result, true
}

// rlParseClock parses "H:MM" or "H" with optional am/pm into 24-hour hour/min.
func rlParseClock(clock, ampm string) (hour, min int, ok bool) {
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

// rlResetLocation resolves a captured timezone name, falling back to local.
func rlResetLocation(tzName string) *time.Location {
	if tzName == "" {
		return time.Local
	}
	if loc, err := time.LoadLocation(tzName); err == nil {
		return loc
	}
	return time.Local
}
