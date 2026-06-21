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
