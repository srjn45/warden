package cli

import (
	"strings"
	"testing"

	"github.com/srjn45/warden/internal/savings"
)

func TestHumanCount(t *testing.T) {
	cases := map[int]string{
		0:         "0",
		12:        "12",
		999:       "999",
		3400:      "3.4k",
		1_200_000: "1.2M",
	}
	for in, want := range cases {
		if got := humanCount(in); got != want {
			t.Errorf("humanCount(%d)=%q, want %q", in, got, want)
		}
	}
}

func TestLeanFactor(t *testing.T) {
	if got := leanFactor(4200, 2600); got != "1.6×" {
		t.Errorf("leanFactor(4200,2600)=%q, want 1.6×", got)
	}
	if got := leanFactor(1000, 0); got != "∞×" {
		t.Errorf("leanFactor with nothing kept=%q, want ∞×", got)
	}
}

func TestFormatBenchmarkEmpty(t *testing.T) {
	out := formatBenchmark(&savings.Summary{}, "")
	if !strings.Contains(out, "no savings recorded yet (all time)") {
		t.Fatalf("empty benchmark missing guidance, got:\n%s", out)
	}
}

func TestFormatBenchmark(t *testing.T) {
	sum := &savings.Summary{
		Events:       3,
		RawTokens:    4200,
		KeptTokens:   2600,
		SavedTokens:  1600,
		SavedDollars: 0.008,
		ReductionPct: 38.1,
		Features: []savings.FeatureSummary{
			{Feature: "check", SavedTokens: 1200, RawTokens: 3000, Events: 1},
			{Feature: "commit", SavedTokens: 400, RawTokens: 1200, Events: 2},
		},
	}
	out := formatBenchmark(sum, "7d")

	for _, want := range []string{
		"warden A/B — since 7d · 3 events",
		"without warden",
		"with warden",
		"4.2k tokens",        // raw counterfactual
		"2.6k tokens",        // kept reality
		"38.1% less context", // reduction headline
		"1.6× leaner",        // raw/kept multiplier
		"$0.01 saved",        // SavedDollars rounded
		"driven by:",
		"check",
		"commit",
		"(75%)", // check's share of saved tokens (1200/1600)
	} {
		if !strings.Contains(out, want) {
			t.Errorf("benchmark output missing %q, got:\n%s", want, out)
		}
	}
}
