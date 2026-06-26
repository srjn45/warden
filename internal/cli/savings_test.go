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

// mixedAxisSummary models a window with both context-axis features (check,
// commit — kept>0) and an offload-axis feature (llm_offload — kept==0), with the
// axis aggregates set as Summarize would compute them.
func mixedAxisSummary() *savings.Summary {
	return &savings.Summary{
		Events:       4,
		RawTokens:    5200,
		KeptTokens:   2600,
		SavedTokens:  2600,
		SavedDollars: 0.0235,
		ReductionPct: 50.0,
		// context axis: check+commit, raw 4200 / kept 2600 / saved 1600.
		ContextRawTokens:    4200,
		ContextKeptTokens:   2600,
		ContextSavedTokens:  1600,
		ContextSavedDollars: 0.008,
		ContextReductionPct: 38.1,
		// offload axis: 1000 input over 1 call.
		OffloadedTokens:  1000,
		OffloadedDollars: 0.0055,
		OffloadedEvents:  1,
		Features: []savings.FeatureSummary{
			{Feature: "check", SavedTokens: 1200, RawTokens: 3000, Events: 1},
			{Feature: "llm_offload", SavedTokens: 1000, RawTokens: 1000, Events: 1},
			{Feature: "commit", SavedTokens: 400, RawTokens: 1200, Events: 2},
		},
	}
}

func TestFormatBenchmark(t *testing.T) {
	out := formatBenchmark(mixedAxisSummary(), "7d")

	for _, want := range []string{
		"warden A/B — since 7d · 4 events",
		"without warden",
		"with warden",
		"4.2k tokens",        // CONTEXT raw counterfactual (not the blended 5.2k)
		"2.6k tokens",        // context kept reality
		"38.1% less context", // context reduction headline
		"1.6× leaner",        // context raw/kept multiplier (4200/2600)
		"$0.01 saved",        // ContextSavedDollars rounded
		// offload reported on its own line, with the assumption flagged.
		"offloaded entirely",
		"1 calls",
		"$0.01 of Claude work", // OffloadedDollars rounded
		"output volume assumed, not measured",
		"driven by:",
		"check",
		"commit",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("benchmark output missing %q, got:\n%s", want, out)
		}
	}

	// The blended 5.2k raw must NOT appear in the A/B block — that axis is context-only.
	if strings.Contains(out, "5.2k tokens") {
		t.Errorf("benchmark A/B leaked the blended raw total, got:\n%s", out)
	}
}

func TestFormatSavingsTwoAxes(t *testing.T) {
	out := formatSavings(mixedAxisSummary(), "7d")

	for _, want := range []string{
		"agent context kept 38.1% leaner", // context claim
		"$0.01 saved",                     // context dollars
		"3 events",                        // context events = 4 total - 1 offload
		"$0.01 of Claude work offloaded entirely", // separate offload claim
		"1 calls",
		"output volume assumed, not measured", // assumption surfaced, never measured
	} {
		if !strings.Contains(out, want) {
			t.Errorf("savings output missing %q, got:\n%s", want, out)
		}
	}
}

func TestFormatSavingsEmpty(t *testing.T) {
	out := formatSavings(&savings.Summary{}, "")
	if !strings.Contains(out, "no savings recorded yet (all time)") {
		t.Fatalf("empty savings missing guidance, got:\n%s", out)
	}
}
