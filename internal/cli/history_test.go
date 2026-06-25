package cli

import (
	"testing"
	"time"
)

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		in   string
		want time.Time
		err  bool
	}{
		{"", time.Time{}, false},
		{"24h", now.Add(-24 * time.Hour), false},
		{"90m", now.Add(-90 * time.Minute), false},
		{"7d", now.Add(-7 * 24 * time.Hour), false},
		{"2w", now.Add(-14 * 24 * time.Hour), false},
		{"2026-06-01", time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), false},
		{"2026-06-01T08:00:00Z", time.Date(2026, 6, 1, 8, 0, 0, 0, time.UTC), false},
		{"garbage", time.Time{}, true},
		{"5x", time.Time{}, true},
	}
	for _, tt := range tests {
		got, err := parseSince(tt.in, now)
		if tt.err {
			if err == nil {
				t.Errorf("parseSince(%q): want error, got %v", tt.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseSince(%q): unexpected error %v", tt.in, err)
			continue
		}
		if !got.Equal(tt.want) {
			t.Errorf("parseSince(%q): want %v, got %v", tt.in, tt.want, got)
		}
	}
}
