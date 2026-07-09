package poller

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// sampleLimitBanner is the best-known Claude Code limit-banner text: a limit
// phrase co-located with the "resets <time> (<tz>)" reset clause.
//
// TODO(open-question): replace with the VERBATIM banner string captured from a
// live limit hit. Until then the detector's trailing-window + working-veto
// guards keep behavior fail-closed. Keep this in sync with claudeLimitBannerRe.
const sampleLimitBanner = "Claude usage limit reached · resets 1:30pm (Europe/Madrid)"

func TestLimitBannerPresent_TracksDetectRateLimit(t *testing.T) {
	// LimitBannerPresent is a thin wrapper; it must agree with detectRateLimit's
	// boolean for both a real trailing banner and plain agent output.
	present := "working...\n" + sampleLimitBanner
	absent := "func detectRateLimit() {} // mentions rate limit\n❯ esc to interrupt"

	wantPresent, _, _ := detectRateLimit(present)
	require.True(t, wantPresent)
	require.Equal(t, wantPresent, LimitBannerPresent(present))

	wantAbsent, _, _ := detectRateLimit(absent)
	require.False(t, wantAbsent)
	require.Equal(t, wantAbsent, LimitBannerPresent(absent))
}

func TestDetectRateLimit_KeywordDetection(t *testing.T) {
	tests := []struct {
		name      string
		pane      string
		wantLimit bool
	}{
		{
			name:      "rate limit banner",
			pane:      "Error: rate limit reached, resets 1:30pm (Europe/Madrid)",
			wantLimit: true,
		},
		{
			name:      "usage limit banner",
			pane:      "Usage limit reached. resets 13:30 (Europe/Madrid)",
			wantLimit: true,
		},
		{
			name:      "session limit banner",
			pane:      "Session limit hit — resets 2:00am (Europe/Madrid)",
			wantLimit: true,
		},
		{
			name:      "quota exceeded banner",
			pane:      "Quota exceeded for this session, resets 9:00 (UTC)",
			wantLimit: true,
		},
		{
			name:      "case insensitive",
			pane:      "RATE LIMIT EXCEEDED — RESETS 1:30PM (Europe/Madrid)",
			wantLimit: true,
		},
		{
			name:      "keyword without reset clause does not match",
			pane:      "Error: rate limit exceeded",
			wantLimit: false,
		},
		{
			name:      "no rate limit",
			pane:      "Working on your request...",
			wantLimit: false,
		},
		{
			name:      "rate in different context",
			pane:      "The success rate is high",
			wantLimit: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotLimit, _, _ := detectRateLimit(tt.pane)
			if gotLimit != tt.wantLimit {
				t.Errorf("detectRateLimit() limit = %v, want %v", gotLimit, tt.wantLimit)
			}
		})
	}
}

func TestDetectRateLimit_TrailingBannerMatches(t *testing.T) {
	// Trailing lines are the real banner (limit phrase + reset clause).
	pane := "working...\n" + sampleLimitBanner
	got, _, _ := detectRateLimit(pane)
	require.True(t, got, "real trailing banner must be detected")
}

func TestDetectRateLimit_AgentOutputDoesNotMatch(t *testing.T) {
	// Agent is writing/reviewing rate-limit code; words appear but no banner shape.
	pane := `func detectRateLimit(pane string) {
  // matches "rate limit", "usage limit", "session limit"
}
❯ esc to interrupt`
	got, _, _ := detectRateLimit(pane)
	require.False(t, got, "agent output mentioning limits must not match")
}

func TestDetectRateLimit_BannerScrolledAwayDoesNotMatch(t *testing.T) {
	// Banner appeared earlier but newer output pushed it out of the trailing window.
	pane := sampleLimitBanner + strings.Repeat("\nnormal work line", 20)
	got, _, _ := detectRateLimit(pane)
	require.False(t, got, "banner outside the trailing window must not match")
}

// sampleSpendBanner is the best-known Claude Code monthly-spend-cap banner: it
// carries NO reset time, so detection must classify it as limited while
// reporting no parseable restore time.
//
// TODO(open-question): replace with the VERBATIM banner captured from a live
// spend-cap hit. Keep in sync with claudeSpendLimitRe.
const sampleSpendBanner = "You've hit your monthly spend limit · raise it at claude.ai/settings/usage/usage-credits to adjust your monthly spend limit."

func TestDetectRateLimit_SpendBannerIsLimitedWithNoTime(t *testing.T) {
	pane := "working...\n" + sampleSpendBanner
	limited, restore, ok := detectRateLimit(pane)
	require.True(t, limited, "spend-cap banner must classify as rate-limited")
	require.False(t, ok, "spend-cap banner carries no parseable reset time")
	require.True(t, restore.IsZero(), "spend-cap restore time must be zero")
}

func TestSpendLimitBannerPresent(t *testing.T) {
	require.True(t, SpendLimitBannerPresent("noise\n"+sampleSpendBanner))
	require.False(t, SpendLimitBannerPresent(sampleLimitBanner),
		"the resettable usage banner is not a spend cap")
	require.False(t, SpendLimitBannerPresent("we discussed the monthly budget earlier"),
		"prose about budgets must not match")
}

func TestSpendBannerScrolledAwayDoesNotMatch(t *testing.T) {
	pane := sampleSpendBanner + strings.Repeat("\nnormal work line", 20)
	require.False(t, SpendLimitBannerPresent(pane),
		"spend banner outside the trailing window must not match")
}

// sampleLimitMenu reproduces Claude's rate-limit choice menu: a ❯ cursor on the
// safe "Stop and wait" option plus an "Upgrade your plan" alternative.
const sampleLimitMenu = "What do you want to do?\n" +
	"❯ 1. Stop and wait for limit to reset\n" +
	"  2. Upgrade your plan\n" +
	"Enter to confirm · Esc to cancel"

func TestLimitMenuOption_SelectsWaitChoice(t *testing.T) {
	idx, ok := LimitMenuOption(sampleLimitMenu)
	require.True(t, ok, "the rate-limit menu must be recognized")
	require.Equal(t, 1, idx, "the wait-for-reset choice is option 1")
}

func TestLimitMenuOption_FindsReorderedWaitChoice(t *testing.T) {
	// The index is looked up by label, not assumed to be 1.
	pane := "What do you want to do?\n" +
		"❯ 1. Upgrade your plan\n" +
		"  2. Stop and wait for limit to reset\n"
	idx, ok := LimitMenuOption(pane)
	require.True(t, ok)
	require.Equal(t, 2, idx, "wait option must be found at its real position")
}

func TestLimitMenuOption_IgnoresNonMenuPane(t *testing.T) {
	_, ok := LimitMenuOption("just some agent output\n❯ esc to interrupt")
	require.False(t, ok, "a non-menu pane must not be treated as the limit menu")
	_, ok = LimitMenuOption(sampleSpendBanner)
	require.False(t, ok, "the spend banner is not the choice menu")
}

func TestLimitMenuSelection_HighlightAware(t *testing.T) {
	// The sample menu pre-highlights option 1 (the wait choice) with ❯.
	idx, highlighted, ok := LimitMenuSelection(sampleLimitMenu)
	require.True(t, ok)
	require.Equal(t, 1, idx)
	require.True(t, highlighted, "the wait option carries the ❯ cursor in the sample menu")

	// Reordered so the wait option is NOT the highlighted one: Enter would confirm
	// the wrong choice, so highlighted must report false.
	reordered := "What do you want to do?\n" +
		"❯ 1. Upgrade your plan\n" +
		"  2. Stop and wait for limit to reset\n"
	idx, highlighted, ok = LimitMenuSelection(reordered)
	require.True(t, ok)
	require.Equal(t, 2, idx)
	require.False(t, highlighted, "wait option is not the highlighted one here")

	if _, _, ok := LimitMenuSelection("just some agent output\n❯ esc to interrupt"); ok {
		t.Fatal("a non-menu pane must not be treated as the limit menu")
	}
}

func TestParseRestoreTime_WeeklyWeekday(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/Madrid")
	tests := []struct {
		name     string
		pane     string
		wantDay  time.Weekday
		wantHour int
		wantMin  int
	}{
		{"weekday with am time and tz", "Weekly limit reached · resets Thursday at 9am (Europe/Madrid)", time.Thursday, 9, 0},
		{"weekday abbrev with 24h time", "resets Thu 14:30 (Europe/Madrid)", time.Thursday, 14, 30},
		{"weekday only, no time", "resets Monday (Europe/Madrid)", time.Monday, 0, 0},
		{"weekday with 'on' and no tz", "resets on Sunday at 6:00pm", time.Sunday, 18, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseRestoreTime(tt.pane)
			require.True(t, ok, "weekly weekday banner must parse")
			require.True(t, got.After(time.Now()), "resume time must be in the future")
			require.Equal(t, tt.wantDay, got.Weekday(), "parsed weekday")
			require.Equal(t, tt.wantHour, got.Hour(), "parsed hour")
			require.Equal(t, tt.wantMin, got.Minute(), "parsed minute")
			if strings.Contains(tt.pane, "Madrid") {
				require.Equal(t, tt.wantHour, got.In(loc).Hour(), "hour is in the banner's zone")
			}
		})
	}
}

func TestParseRestoreTime_WeeklyCalendarDate(t *testing.T) {
	tests := []struct {
		name      string
		pane      string
		wantMonth time.Month
		wantDay   int
		wantHour  int
		wantMin   int
	}{
		{"month day with time and tz", "resets Jul 14 at 3pm (UTC)", time.July, 14, 15, 0},
		{"full month name, no time", "resets July 14", time.July, 14, 0, 0},
		{"day month order", "resets 14 Jul at 09:30 (UTC)", time.July, 14, 9, 30},
		{"explicit future year", "resets Jan 2, 2099 (UTC)", time.January, 2, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseRestoreTime(tt.pane)
			require.True(t, ok, "weekly calendar-date banner must parse")
			require.True(t, got.After(time.Now()), "resume time must be in the future")
			require.Equal(t, tt.wantMonth, got.Month(), "parsed month")
			require.Equal(t, tt.wantDay, got.Day(), "parsed day")
			require.Equal(t, tt.wantHour, got.Hour(), "parsed hour")
			require.Equal(t, tt.wantMin, got.Minute(), "parsed minute")
		})
	}
}

func TestParseRestoreTime_SessionClockStillWins(t *testing.T) {
	// The precise HH:mm session format must still take priority over the looser
	// weekday/date parsers (it carries an explicit zone).
	loc, _ := time.LoadLocation("Europe/Madrid")
	got, ok := ParseRestoreTime("resets 13:30 (Europe/Madrid)")
	require.True(t, ok)
	require.Equal(t, 13, got.In(loc).Hour())
	require.Equal(t, 30, got.In(loc).Minute())
}

func TestParseRestoreTime_NoTimeInMessage(t *testing.T) {
	_, ok := ParseRestoreTime("Rate limit exceeded. Try again later.")
	require.False(t, ok, "a message with no clock-time must not parse")
}

func TestParseRestoreTime_RollsPastTimeToTomorrow(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/Madrid")
	now := time.Now().In(loc)
	// Pick a clock-time one hour BEFORE now → must roll to tomorrow, not return now.
	past := now.Add(-1 * time.Hour)
	pane := "resets " + past.Format("15:04") + " (Europe/Madrid)"
	got, ok := ParseRestoreTime(pane)
	require.True(t, ok)
	require.True(t, got.After(time.Now()), "past clock-time must roll forward, not return now")
	require.WithinDuration(t, now.Add(23*time.Hour), got, 90*time.Minute)
}

func TestParseRestoreTime_AmPmFromGroup(t *testing.T) {
	// An unrelated 'pm'/'am' elsewhere in the pane must not flip the parse.
	loc, _ := time.LoadLocation("Europe/Madrid")
	pane := "the pm reviewed it; resets 1:30am (Europe/Madrid)"
	got, ok := ParseRestoreTime(pane)
	require.True(t, ok)
	require.Equal(t, 1, got.In(loc).Hour())
}

func TestParseRestoreTime_24Hour(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/Madrid")
	pane := "resets 13:30 (Europe/Madrid)"
	got, ok := ParseRestoreTime(pane)
	require.True(t, ok)
	require.Equal(t, 13, got.In(loc).Hour())
}

func TestParseRestoreTime_ZonelessNeverBeforeNow(t *testing.T) {
	got, ok := ParseRestoreTime("try again at 00:01")
	require.True(t, ok)
	require.False(t, got.Before(time.Now()), "zone-less fallback must never return a past time")
}
