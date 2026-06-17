package poller

import (
	"testing"
)

func TestDetectRateLimit_KeywordDetection(t *testing.T) {
	tests := []struct {
		name      string
		pane      string
		wantLimit bool
	}{
		{
			name:      "rate limit keyword",
			pane:      "Error: rate limit exceeded",
			wantLimit: true,
		},
		{
			name:      "usage limit keyword",
			pane:      "Usage limit reached. Try again later.",
			wantLimit: true,
		},
		{
			name:      "session limit keyword",
			pane:      "Session limit hit",
			wantLimit: true,
		},
		{
			name:      "quota exceeded keyword",
			pane:      "Quota exceeded for this session",
			wantLimit: true,
		},
		{
			name:      "case insensitive",
			pane:      "RATE LIMIT EXCEEDED",
			wantLimit: true,
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

func TestDetectRateLimit_WithBuriedKeyword(t *testing.T) {
	pane := `Previous output line 1
Previous output line 2
Error: rate limit exceeded. Try again later.
More output
❯ Continue?`

	gotLimit, _, _ := detectRateLimit(pane)
	if !gotLimit {
		t.Error("should detect rate limit even when buried in output")
	}
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
