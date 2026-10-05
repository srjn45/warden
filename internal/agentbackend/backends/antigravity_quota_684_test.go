package backends

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// agyLivePane684 is the exact pane tail captured live in GitHub #684: the banner
// sits 7 lines from the bottom, above the input box.
const agyLivePane684 = "⚠ Individual quota reached. Please upgrade your subscription to increase your limits. Resets in 10m5s.\n" +
	"Error ID: 579fd7be-0000-0000-0000-000000000000\n" +
	"\n" +
	"────────────────\n" +
	">\n" +
	"────────────────\n" +
	"? for shortcuts                                   Gemini 3.8 Flash · medium\n"

func TestAntigravityDetectRateLimit_Issue684Capture(t *testing.T) {
	before := time.Now()
	limited, at, ok := Antigravity{}.DetectRateLimit(agyLivePane684)
	require.True(t, limited)
	require.True(t, ok)
	require.WithinDuration(t, before.Add(10*time.Minute+5*time.Second), at, 5*time.Second)
}

func TestAntigravityDetectRateLimit_BannerVariants(t *testing.T) {
	box := "\n────────────────\n>\n────────────────\n? for shortcuts   Gemini 3.8 Flash · medium\n"
	cases := []struct {
		name  string
		pane  string
		limit bool
		want  time.Duration // zero = reset unknown/absolute (not asserted)
	}{
		{"seconds only", "⚠ Individual quota reached. Resets in 45s." + box, true, 45 * time.Second},
		{"hours minutes", "⚠ Individual quota reached. Resets in 2h3m." + box, true, 2*time.Hour + 3*time.Minute},
		{"hms", "⚠ Individual quota reached. Resets in 1h2m3s." + box, true, time.Hour + 2*time.Minute + 3*time.Second},
		{"spaced words", "⚠ Quota reached. Resets in 2 hours 3 minutes." + box, true, 2*time.Hour + 3*time.Minute},
		{"fractional", "⚠ Rate limited. Resets in 1.5h." + box, true, 90 * time.Minute},
		{"extra lines", "⚠ Individual quota reached. Resets in 10m5s.\nError ID: x\nhint a\nhint b\n" + box, true, 10*time.Minute + 5*time.Second},
		{"absolute still works", "⚠ Free-tier session quota reached.\n  Available at 15:30" + box, true, 0},
		{"no clause", "⚠ Individual quota reached. Please upgrade." + box, false, 0},
		{"resets in without phrase", "Cache resets in 10m after deploy." + box, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := time.Now()
			limited, at, ok := Antigravity{}.DetectRateLimit(tc.pane)
			require.Equal(t, tc.limit, limited)
			if tc.want > 0 {
				require.True(t, ok)
				require.WithinDuration(t, before.Add(tc.want), at, 5*time.Second)
			}
		})
	}
}

func TestRLParseRelativeReset(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	got, ok := rlParseRelativeReset("Resets in 10m5s.", now)
	require.True(t, ok)
	require.Equal(t, now.Add(10*time.Minute+5*time.Second), got)
	_, ok = rlParseRelativeReset("Resets at 10:05", now)
	require.False(t, ok)
	_, ok = rlParseRelativeReset("Resets in 0s", now)
	require.False(t, ok)
}

func TestAntigravityDetectRateLimit_TailWindow(t *testing.T) {
	box := "\n────────────────\n>\n────────────────\n? for shortcuts   Gemini 3.8 Flash · medium\n"
	banner := "⚠ Individual quota reached. Resets in 10m5s.\nError ID: x\n"

	// Positive: banner well above the legacy 6-line window but still directly above the box.
	limited, _, _ := Antigravity{}.DetectRateLimit(banner + "\n\nextra\n" + box)
	require.True(t, limited)

	// Negative: old banner followed by lots of later output (box still at the bottom).
	stale := banner + strings.Repeat("later work line\n", 20) + box
	limited, _, _ = Antigravity{}.DetectRateLimit(stale)
	require.False(t, limited, "banner high in scrollback must not be a live limit")

	// Negative: no box and banner outside the legacy tail.
	limited, _, _ = Antigravity{}.DetectRateLimit(banner + strings.Repeat("line\n", 10))
	require.False(t, limited)
}

func TestAntigravityObservedQuotaScope(t *testing.T) {
	m, s, ok := Antigravity{}.ObservedQuotaScope(agyLivePane684)
	require.True(t, ok)
	require.Equal(t, "Gemini 3.8 Flash", m)
	require.Equal(t, "gemini", s)

	claude := strings.Replace(agyLivePane684, "Gemini 3.8 Flash · medium", "Claude Opus 4.6 (Thinking) · high", 1)
	m, s, ok = Antigravity{}.ObservedQuotaScope(claude)
	require.True(t, ok)
	require.Equal(t, "non-gemini", s)
	require.Contains(t, m, "Claude Opus")

	for name, pane := range map[string]string{
		"no box":         "Gemini 3.8 Flash · medium\n",
		"unknown model":  strings.Replace(agyLivePane684, "Gemini 3.8 Flash", "Mystery 1", 1),
		"no status":      "hello\n────────────────\n>\n────────────────\n? for shortcuts\n",
		"quoted in body": "Gemini 3.8 Flash · medium\nfoo\n────────────────\n>\n────────────────\n? for shortcuts\n",
	} {
		_, _, ok := Antigravity{}.ObservedQuotaScope(pane)
		require.False(t, ok, name)
	}
}
