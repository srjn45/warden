package spend

import (
	"math"
	"testing"
)

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestPriceForFamilies(t *testing.T) {
	cases := []struct {
		model   string
		wantIn  float64
		wantOut float64
	}{
		{"opus", 5, 25},
		{"claude-opus-4-8", 5, 25},
		{"us.anthropic.claude-opus-4", 5, 25},
		{"sonnet", 3, 15},
		{"claude-sonnet-4-6", 3, 15},
		{"haiku", 0.8, 4},
		{"claude-haiku-4-5-20251001", 0.8, 4},
		{"claude-fable-5", 5, 25},
		{"", 5, 25},             // unknown/empty → Opus default
		{"gpt-whatever", 5, 25}, // unrecognized → Opus default (conservative)
	}
	for _, c := range cases {
		p := PriceFor(c.model)
		if !approx(p.InputPerMTok, c.wantIn) || !approx(p.OutputPerMTok, c.wantOut) {
			t.Errorf("PriceFor(%q) = %+v, want in=%v out=%v", c.model, p, c.wantIn, c.wantOut)
		}
	}
}

func TestCost(t *testing.T) {
	// 1M input + 1M output on Opus = $5 + $25 = $30.
	if got := Cost("opus", 1_000_000, 1_000_000); !approx(got, 30) {
		t.Errorf("Cost opus 1M/1M = %v, want 30", got)
	}
	// Sonnet 2M input only = $6.
	if got := Cost("sonnet", 2_000_000, 0); !approx(got, 6) {
		t.Errorf("Cost sonnet 2M/0 = %v, want 6", got)
	}
	if got := Cost("haiku", 0, 0); got != 0 {
		t.Errorf("Cost of nothing = %v, want 0", got)
	}
}
