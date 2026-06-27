package cli

import (
	"strings"
	"testing"

	"github.com/srjn45/warden/internal/spend"
)

func sampleSpendReport() *spend.Report {
	return &spend.Report{
		TotalUSD:     8,
		InputTokens:  2_000_000,
		OutputTokens: 0,
		DailyUSD:     5,
		WeeklyUSD:    8,
		ByAgent:      []spend.Bucket{{Key: "a1", Input: 1_000_000, USD: 5}, {Key: "a2", Input: 1_000_000, USD: 3}},
		ByRepo:       []spend.Bucket{{Key: "/x", Input: 2_000_000, USD: 8}},
		ByDay:        []spend.Bucket{{Key: "2026-06-27", Input: 2_000_000, USD: 5}},
	}
}

func TestFormatSpendAllRollups(t *testing.T) {
	out := formatSpend(sampleSpendReport(), "")
	for _, want := range []string{"$8.00 total", "$5.00 today", "$8.00 this week", "by agent", "by repo", "by day", "a1", "$5.00", "/x"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestFormatSpendByFilter(t *testing.T) {
	out := formatSpend(sampleSpendReport(), "agent")
	if !strings.Contains(out, "by agent") {
		t.Errorf("expected agent rollup:\n%s", out)
	}
	if strings.Contains(out, "by repo") || strings.Contains(out, "by day") {
		t.Errorf("--by agent should not show other rollups:\n%s", out)
	}
}

func TestFormatSpendEmpty(t *testing.T) {
	out := formatSpend(&spend.Report{}, "")
	if !strings.Contains(out, "no spend measured yet") {
		t.Errorf("empty report should explain nothing measured:\n%s", out)
	}
}
