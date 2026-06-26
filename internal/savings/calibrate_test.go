package savings

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// fakeCounter is a deterministic, no-network TokenCounter for the derivation
// tests: it reports a fixed bytes-per-token ratio (tokens = len/ratio, rounded
// up) and tallies how many times it was called, so tests can assert the paid-call
// bound. An optional failAt makes the Nth call (1-based) return an error.
type fakeCounter struct {
	ratio  float64
	calls  int
	failAt int
}

func (f *fakeCounter) CountTokens(_ context.Context, text string) (int, error) {
	f.calls++
	if f.failAt != 0 && f.calls == f.failAt {
		return 0, errors.New("boom")
	}
	t := int(float64(len(text))/f.ratio + 0.999) // ceil-ish
	if t < 1 && len(text) > 0 {
		t = 1
	}
	return t, nil
}

func TestDeriveCalibration_Math(t *testing.T) {
	// Two samples: 12 bytes and 8 bytes. At a 4 bytes/token counter they count to
	// 3 and 2 tokens → factor = (12+8)/(3+2) = 4.0 exactly.
	counter := &fakeCounter{ratio: 4}
	cal, err := DeriveCalibration(context.Background(), counter, []string{"123456789012", "12345678"}, 50)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	if cal.Samples != 2 {
		t.Fatalf("samples = %d, want 2", cal.Samples)
	}
	if cal.SampleBytes != 20 || cal.SampleTokens != 5 {
		t.Fatalf("bytes/tokens = %d/%d, want 20/5", cal.SampleBytes, cal.SampleTokens)
	}
	if cal.BytesPerToken != 4.0 {
		t.Fatalf("factor = %v, want 4.0", cal.BytesPerToken)
	}
	if cal.Model != CalibrationModel {
		t.Fatalf("model = %q, want %q", cal.Model, CalibrationModel)
	}
}

func TestDeriveCalibration_DerivesNonHeuristicRatio(t *testing.T) {
	// A workload that tokenizes denser than 4 bytes/token (e.g. 6) must yield a
	// factor materially different from the heuristic's 4 — that is the whole point.
	counter := &fakeCounter{ratio: 6}
	cal, err := DeriveCalibration(context.Background(), counter, []string{"aaaaaaaaaaaa", "bbbbbb"}, 50)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	// 12 bytes → 2 tokens, 6 bytes → 1 token → 18/3 = 6.0.
	if cal.BytesPerToken != 6.0 {
		t.Fatalf("factor = %v, want 6.0", cal.BytesPerToken)
	}
}

func TestDeriveCalibration_BoundsPaidCalls(t *testing.T) {
	counter := &fakeCounter{ratio: 4}
	samples := []string{"aaaa", "bbbb", "cccc", "dddd", "eeee"}
	cal, err := DeriveCalibration(context.Background(), counter, samples, 2)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	if counter.calls != 2 {
		t.Fatalf("counter called %d times, want 2 (maxCalls cap)", counter.calls)
	}
	if cal.Samples != 2 {
		t.Fatalf("samples = %d, want 2", cal.Samples)
	}
}

func TestDeriveCalibration_SkipsEmptySamples(t *testing.T) {
	counter := &fakeCounter{ratio: 4}
	cal, err := DeriveCalibration(context.Background(), counter, []string{"", "12345678", ""}, 50)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	if counter.calls != 1 {
		t.Fatalf("counter called %d times, want 1 (empties skipped)", counter.calls)
	}
	if cal.Samples != 1 {
		t.Fatalf("samples = %d, want 1", cal.Samples)
	}
}

func TestDeriveCalibration_CounterErrorIsFatal(t *testing.T) {
	// A per-sample error must abort — a partial sum would bias the factor.
	counter := &fakeCounter{ratio: 4, failAt: 2}
	if _, err := DeriveCalibration(context.Background(), counter, []string{"aaaa", "bbbb", "cccc"}, 50); err == nil {
		t.Fatal("expected error from failing counter, got nil")
	}
}

func TestDeriveCalibration_NoUsableSamples(t *testing.T) {
	counter := &fakeCounter{ratio: 4}
	if _, err := DeriveCalibration(context.Background(), counter, []string{"", ""}, 50); err == nil {
		t.Fatal("expected error with no usable samples, got nil")
	}
	if counter.calls != 0 {
		t.Fatalf("counter called %d times, want 0", counter.calls)
	}
}

func TestDeriveCalibration_GuardsBadArgs(t *testing.T) {
	if _, err := DeriveCalibration(context.Background(), nil, []string{"x"}, 50); err == nil {
		t.Fatal("expected error for nil counter")
	}
	if _, err := DeriveCalibration(context.Background(), &fakeCounter{ratio: 4}, []string{"x"}, 0); err == nil {
		t.Fatal("expected error for non-positive maxCalls")
	}
}

// TestEstimateTokensLen_UncalibratedEquivalence locks in that, with no factor
// set, EstimateTokensLen is byte-for-byte the prior (n+3)/4 heuristic — so every
// offline command behaves exactly as before calibration existed.
func TestEstimateTokensLen_UncalibratedEquivalence(t *testing.T) {
	ClearCalibration() // ensure no prior test left a factor installed
	for _, n := range []int{0, -5, 1, 3, 4, 5, 7, 8, 100, 4000, 4001} {
		if got, want := EstimateTokensLen(n), (func() int {
			if n <= 0 {
				return 0
			}
			return (n + 3) / 4
		}()); got != want {
			t.Errorf("uncalibrated EstimateTokensLen(%d) = %d, want %d", n, got, want)
		}
	}
}

// TestEstimateTokensLen_Calibrated checks the calibrated path divides bytes by
// the measured factor (rounded), floors a non-empty input at one token, and that
// ClearCalibration restores the heuristic.
func TestEstimateTokensLen_Calibrated(t *testing.T) {
	defer ClearCalibration()
	SetCalibration(5.0) // 5 bytes per token
	cases := []struct {
		n    int
		want int
	}{
		{0, 0},
		{1, 1}, // floors to 1, not round(0.2)=0
		{2, 1}, // round(0.4)=0 → floored to 1
		{5, 1}, // round(1.0)=1
		{8, 2}, // round(1.6)=2
		{100, 20},
		{4000, 800}, // would be 1000 under the heuristic — calibration changes the figure
	}
	for _, c := range cases {
		if got := EstimateTokensLen(c.n); got != c.want {
			t.Errorf("calibrated(5.0) EstimateTokensLen(%d) = %d, want %d", c.n, got, c.want)
		}
	}
	// SetCalibration ignores non-positive values (never replaces a measurement with garbage).
	SetCalibration(-1)
	if got := EstimateTokensLen(4000); got != 800 {
		t.Errorf("SetCalibration(-1) should be a no-op; got %d, want 800", got)
	}
	ClearCalibration()
	if got := EstimateTokensLen(4000); got != 1000 {
		t.Errorf("after ClearCalibration EstimateTokensLen(4000) = %d, want 1000 (heuristic)", got)
	}
}

func TestSaveLoadCalibration_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	if _, ok, err := LoadCalibration(dir); err != nil || ok {
		t.Fatalf("uncalibrated dir: ok=%v err=%v, want ok=false err=nil", ok, err)
	}
	want := Calibration{BytesPerToken: 3.7, Samples: 12, SampleBytes: 370, SampleTokens: 100, Model: CalibrationModel}
	if err := SaveCalibration(dir, want); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, ok, err := LoadCalibration(dir)
	if err != nil || !ok {
		t.Fatalf("load: ok=%v err=%v, want ok=true", ok, err)
	}
	if got.BytesPerToken != want.BytesPerToken || got.Samples != want.Samples {
		t.Fatalf("round-trip mismatch: %+v vs %+v", got, want)
	}
	// The factor must land where the daemon's Store.Calibration() looks for it:
	// filepath.Dir of the ledger path resolves back to dir.
	if _, ok, err := LoadCalibration(filepath.Dir(filepath.Join(dir, "ledger.jsonl"))); err != nil || !ok {
		t.Fatalf("load from ledger dir: ok=%v err=%v", ok, err)
	}
}

func TestLoadCalibration_NonPositiveTreatedAbsent(t *testing.T) {
	dir := t.TempDir()
	if err := SaveCalibration(dir, Calibration{BytesPerToken: 0}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, ok, err := LoadCalibration(dir); err != nil || ok {
		t.Fatalf("non-positive factor: ok=%v err=%v, want ok=false (absent)", ok, err)
	}
}
