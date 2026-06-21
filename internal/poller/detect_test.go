package poller

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// sampleLimitBanner is the best-known Claude Code limit-banner text: a limit
// phrase co-located with the "resets <time> (<tz>)" reset clause.
//
// TODO(open-question): replace with the VERBATIM banner string captured from a
// live limit hit. Until then the detector's trailing-window + working-veto
// guards keep behavior fail-closed. Keep this in sync with claudeLimitBannerRe.
const sampleLimitBanner = "Claude usage limit reached · resets 1:30pm (Europe/Madrid)"

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

func TestParseRestoreTime_Placeholder(t *testing.T) {
	// NOTE: This is a placeholder test until exact message format is known
	// Will be updated when user provides actual Claude Code error message

	tests := []struct {
		name   string
		pane   string
		wantOK bool
	}{
		{
			name:   "no time in message",
			pane:   "Rate limit exceeded. Try again later.",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, gotOK := ParseRestoreTime(tt.pane)
			if gotOK != tt.wantOK {
				t.Errorf("ParseRestoreTime() ok = %v, want %v", gotOK, tt.wantOK)
			}
		})
	}
}
