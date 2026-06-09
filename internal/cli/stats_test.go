package cli

import (
	"strings"
	"testing"

	"github.com/srjn45/warden/internal/metrics"
)

func TestFormatStatsSortsByRSS(t *testing.T) {
	s := &metrics.Sample{
		System: metrics.SystemStats{TotalBytes: 16 << 30, UsedBytes: 8 << 30, PressureLevel: "normal", AgentCount: 2, AttributedRSSBytes: 3 << 30},
		Agents: []metrics.AgentStat{
			{ID: "small", Paneable: true, RSSBytes: 1 << 30, CPUPercent: 1, ProcCount: 2, UptimeSec: 60},
			{ID: "big", Paneable: true, RSSBytes: 2 << 30, CPUPercent: 9, ProcCount: 3, UptimeSec: 3600},
		},
	}
	out := formatStats(s)
	// "big" must appear before "small" (sorted by RSS desc).
	if strings.Index(out, "big") > strings.Index(out, "small") {
		t.Fatalf("expected big before small:\n%s", out)
	}
	if !strings.Contains(out, "pressure") || !strings.Contains(out, "agents") {
		t.Fatalf("summary line missing:\n%s", out)
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[uint64]string{
		0:       "0 B",
		1023:    "1023 B",
		1 << 10: "1.0 KiB",
		1536:    "1.5 KiB",
		1 << 20: "1.0 MiB",
		2 << 30: "2.0 GiB",
	}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Fatalf("humanBytes(%d)=%q want %q", in, got, want)
		}
	}
}
