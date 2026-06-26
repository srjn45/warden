package savings

import (
	"testing"
	"time"
)

func TestEstimateTokens(t *testing.T) {
	cases := []struct {
		n    int
		want int
	}{
		{0, 0},
		{-5, 0}, // negative length is treated as empty, not a negative token count
		{1, 1},
		{4, 1},
		{5, 2},
		{4000, 1000},
	}
	for _, c := range cases {
		if got := EstimateTokensLen(c.n); got != c.want {
			t.Errorf("EstimateTokensLen(%d) = %d, want %d", c.n, got, c.want)
		}
	}
	if got := EstimateTokens([]byte("hello world!")); got != 3 { // 12 bytes → 3 tokens
		t.Errorf("EstimateTokens(12 bytes) = %d, want 3", got)
	}
}

func TestNewEventClampsSaved(t *testing.T) {
	ev := NewEvent(FeatureCheck, "a1", 1000, 40)
	if ev.Saved != 960 {
		t.Errorf("Saved = %d, want 960", ev.Saved)
	}
	if ev.TS.IsZero() {
		t.Error("NewEvent did not stamp TS")
	}
	// kept > raw must never produce a negative saving.
	neg := NewEvent(FeatureCommit, "a1", 10, 50)
	if neg.Saved != 0 {
		t.Errorf("Saved = %d, want 0 (clamped)", neg.Saved)
	}
}

func TestSummarize(t *testing.T) {
	evs := []Event{
		NewEvent(FeatureCheck, "a1", 1000, 100),   // saved 900
		NewEvent(FeatureCheck, "a2", 500, 0),      // saved 500
		NewEvent(FeatureCommit, "a1", 200, 20),    // saved 180
		NewEvent(FeatureLLMOffload, "a1", 300, 0), // saved 300
	}
	sum := Summarize(evs, time.Time{})

	if sum.Events != 4 {
		t.Errorf("Events = %d, want 4", sum.Events)
	}
	if sum.SavedTokens != 900+500+180+300 {
		t.Errorf("SavedTokens = %d, want 1880", sum.SavedTokens)
	}
	if sum.KeptTokens != 100+0+20+0 {
		t.Errorf("KeptTokens = %d, want 120", sum.KeptTokens)
	}
	// ReductionPct = saved / (saved + kept) = 1880 / 2000 = 94%.
	if sum.ReductionPct < 93.9 || sum.ReductionPct > 94.1 {
		t.Errorf("ReductionPct = %.2f, want ~94", sum.ReductionPct)
	}
	// $ = 1880 tokens * $5/MTok.
	wantDollars := 1880.0 * 5.0 / 1_000_000
	if sum.SavedDollars < wantDollars-1e-9 || sum.SavedDollars > wantDollars+1e-9 {
		t.Errorf("SavedDollars = %.8f, want %.8f", sum.SavedDollars, wantDollars)
	}

	// Features sorted by SavedTokens desc: check (1400) > llm_offload (300) > commit (180).
	if len(sum.Features) != 3 {
		t.Fatalf("Features len = %d, want 3", len(sum.Features))
	}
	if sum.Features[0].Feature != FeatureCheck || sum.Features[0].SavedTokens != 1400 {
		t.Errorf("Features[0] = %+v, want check/1400", sum.Features[0])
	}
	if sum.Features[1].Feature != FeatureLLMOffload {
		t.Errorf("Features[1] = %q, want llm_offload", sum.Features[1].Feature)
	}
	if sum.Features[2].Feature != FeatureCommit {
		t.Errorf("Features[2] = %q, want commit", sum.Features[2].Feature)
	}
}

func TestSummarizeEmpty(t *testing.T) {
	sum := Summarize(nil, time.Time{})
	if sum.Events != 0 || sum.SavedTokens != 0 || sum.ReductionPct != 0 {
		t.Errorf("empty summary = %+v, want zero", sum)
	}
	if len(sum.Features) != 0 {
		t.Errorf("Features = %v, want empty", sum.Features)
	}
}
