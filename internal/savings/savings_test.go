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
	ev := NewEvent(FeatureCheck, "a1", 1000, 40, 0)
	if ev.Saved != 960 {
		t.Errorf("Saved = %d, want 960", ev.Saved)
	}
	if ev.TS.IsZero() {
		t.Error("NewEvent did not stamp TS")
	}
	// kept > raw must never produce a negative saving.
	neg := NewEvent(FeatureCommit, "a1", 10, 50, 0)
	if neg.Saved != 0 {
		t.Errorf("Saved = %d, want 0 (clamped)", neg.Saved)
	}
}

func TestNewEventNetsCost(t *testing.T) {
	// Compact: raw 270000 reclaimed context, kept 0 on this axis, minus a measured
	// 1200-token summary-generation cost → NET saved 268800.
	ev := NewEvent(FeatureCompact, "a1", 270000, 0, 1200)
	if ev.Saved != 268800 {
		t.Errorf("Saved = %d, want 268800 (raw-kept-cost)", ev.Saved)
	}
	if ev.CostTokens != 1200 {
		t.Errorf("CostTokens = %d, want 1200", ev.CostTokens)
	}
	// A cost that exceeds the gross saving clamps to 0, never negative.
	over := NewEvent(FeatureCompact, "a1", 100, 0, 500)
	if over.Saved != 0 {
		t.Errorf("Saved = %d, want 0 (cost > gross, clamped)", over.Saved)
	}
}

func TestSummarize(t *testing.T) {
	evs := []Event{
		NewEvent(FeatureCheck, "a1", 1000, 100, 0),   // saved 900
		NewEvent(FeatureCheck, "a2", 500, 0, 0),      // saved 500
		NewEvent(FeatureCommit, "a1", 200, 20, 0),    // saved 180
		NewEvent(FeatureLLMOffload, "a1", 300, 0, 0), // saved 300
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
	// Blended $ is the honest sum of both axes: context input (1580 saved tokens *
	// $5/MTok) + offload (300 input * $5/MTok + 1 event * 64 output * $25/MTok).
	wantDollars := (1580.0*5.0 + 300.0*5.0 + 1*64*25.0) / 1_000_000
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

func TestFeatureAxis(t *testing.T) {
	cases := map[string]string{
		FeatureCheck:      axisContext,
		FeatureCommit:     axisContext,
		FeatureCompact:    axisContext,
		FeatureLLMOffload: axisOffload,
		"some_future":     axisContext, // unknown features default to the context axis
	}
	for feature, want := range cases {
		if got := featureAxis(feature); got != want {
			t.Errorf("featureAxis(%q) = %q, want %q", feature, got, want)
		}
	}
}

func TestSummarizeAxes(t *testing.T) {
	evs := []Event{
		NewEvent(FeatureCheck, "a1", 1000, 100, 0),   // context: saved 900
		NewEvent(FeatureCheck, "a2", 500, 0, 0),      // context: saved 500
		NewEvent(FeatureCommit, "a1", 200, 20, 0),    // context: saved 180
		NewEvent(FeatureLLMOffload, "a1", 300, 0, 0), // offload: 300 input, 1 event
		NewEvent(FeatureLLMOffload, "a2", 700, 0, 0), // offload: 700 input, 1 event
	}
	sum := Summarize(evs, time.Time{})

	// Context axis: raw 1700, kept 120, saved 1580 — over context features ONLY,
	// so the two offload events do not touch this ratio.
	if sum.ContextRawTokens != 1700 || sum.ContextKeptTokens != 120 || sum.ContextSavedTokens != 1580 {
		t.Errorf("context tokens = raw %d/kept %d/saved %d, want 1700/120/1580",
			sum.ContextRawTokens, sum.ContextKeptTokens, sum.ContextSavedTokens)
	}
	// ContextReductionPct = 1580 / (1580 + 120) = 92.94%.
	if sum.ContextReductionPct < 92.9 || sum.ContextReductionPct > 93.0 {
		t.Errorf("ContextReductionPct = %.2f, want ~92.94", sum.ContextReductionPct)
	}
	wantCtxDollars := 1580.0 * 5.0 / 1_000_000
	if sum.ContextSavedDollars < wantCtxDollars-1e-9 || sum.ContextSavedDollars > wantCtxDollars+1e-9 {
		t.Errorf("ContextSavedDollars = %.8f, want %.8f", sum.ContextSavedDollars, wantCtxDollars)
	}

	// Offload axis: 1000 input tokens over 2 events.
	if sum.OffloadedTokens != 1000 || sum.OffloadedEvents != 2 {
		t.Errorf("offload = %d tokens/%d events, want 1000/2", sum.OffloadedTokens, sum.OffloadedEvents)
	}
	// OffloadedDollars = 1000 input * $5/MTok + 2 events * 64 output * $25/MTok.
	wantOffload := (1000.0*5.0 + 2*64*25.0) / 1_000_000
	if sum.OffloadedDollars < wantOffload-1e-9 || sum.OffloadedDollars > wantOffload+1e-9 {
		t.Errorf("OffloadedDollars = %.8f, want %.8f (input + assumed output)", sum.OffloadedDollars, wantOffload)
	}
	// Sanity: the assumed output term is non-trivial, so output pricing is in play.
	if inputOnly := 1000.0 * 5.0 / 1_000_000; sum.OffloadedDollars <= inputOnly {
		t.Errorf("OffloadedDollars = %.8f did not add avoided output (input-only %.8f)", sum.OffloadedDollars, inputOnly)
	}

	// Blended SavedDollars is the honest sum of both axes' priced values.
	if want := wantCtxDollars + wantOffload; sum.SavedDollars < want-1e-9 || sum.SavedDollars > want+1e-9 {
		t.Errorf("SavedDollars = %.8f, want context+offload %.8f", sum.SavedDollars, want)
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
