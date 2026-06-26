package savings

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func mustDay(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return ts
}

func TestBucketByDay(t *testing.T) {
	evs := []Event{
		{TS: mustDay(t, "2026-06-03T08:00:00Z"), Feature: FeatureCommit, Saved: 200}, // out of order on purpose
		{TS: mustDay(t, "2026-06-01T10:00:00Z"), Feature: FeatureCheck, Saved: 100},
		{TS: mustDay(t, "2026-06-01T22:30:00Z"), Feature: FeatureCheck, Saved: 50},
		// A non-UTC zone whose instant lands on 2026-06-01 22:30 UTC (00:30+02:00).
		{TS: mustDay(t, "2026-06-02T00:30:00+02:00"), Feature: FeatureCommit, Saved: 7},
	}
	got := BucketByDay(evs)
	// Two distinct UTC days: 06-01 (three events incl. the +02:00 one) and 06-03.
	if len(got) != 2 {
		t.Fatalf("got %d buckets, want 2: %+v", len(got), got)
	}
	// Oldest first, regardless of input order.
	if got[0].Date != "2026-06-01" || got[0].SavedTokens != 157 || got[0].Events != 3 {
		t.Errorf("bucket[0] = %+v, want 2026-06-01/157/3", got[0])
	}
	if got[1].Date != "2026-06-03" || got[1].SavedTokens != 200 || got[1].Events != 1 {
		t.Errorf("bucket[1] = %+v, want 2026-06-03/200/1", got[1])
	}
}

func TestBucketByDayEmpty(t *testing.T) {
	if got := BucketByDay(nil); len(got) != 0 {
		t.Fatalf("empty events → %+v, want no buckets", got)
	}
}

func TestTruncateSample(t *testing.T) {
	if got := TruncateSample(""); got != "" {
		t.Errorf("empty → %q", got)
	}
	short := "a short sample"
	if got := TruncateSample(short); got != short {
		t.Errorf("under cap should pass through: %q", got)
	}
	big := strings.Repeat("a", sampleCap+500)
	got := TruncateSample(big)
	if len(got) != sampleCap {
		t.Errorf("ascii over cap len = %d, want %d", len(got), sampleCap)
	}
	// Multi-byte input must never be cut mid-rune; result is valid UTF-8 and ≤ cap.
	mb := strings.Repeat("é", sampleCap) // 2 bytes per rune
	gotMB := TruncateSample(mb)
	if len(gotMB) > sampleCap {
		t.Errorf("multibyte over cap len = %d, want ≤ %d", len(gotMB), sampleCap)
	}
	if !utf8.ValidString(gotMB) {
		t.Errorf("truncated multibyte sample is not valid UTF-8")
	}
}

// TestStoreSampleGateOff verifies the default: an emit site may attach a sample,
// but with sampling off the store strips it before writing — the ledger never
// persists raw/kept output unless opted in.
func TestStoreSampleGateOff(t *testing.T) {
	s, err := NewStore(filepath.Join(t.TempDir(), "savings"))
	if err != nil {
		t.Fatal(err)
	}
	// sampling defaults to off (no SetSampling call).
	ev := NewEvent(FeatureCheck, "a1", 1000, 100, 0)
	ev.RawSample, ev.KeptSample = "raw output bytes", "kept summary"
	if err := s.Record(ev); err != nil {
		t.Fatal(err)
	}
	evs, _ := s.Events(time.Time{})
	if len(evs) != 1 {
		t.Fatalf("got %d events", len(evs))
	}
	if evs[0].RawSample != "" || evs[0].KeptSample != "" {
		t.Errorf("samples persisted while gate off: %+v", evs[0])
	}
}

// TestStoreSampleGateOnRetainsSampled verifies that with sampling on, only ~1 in
// sampleEvery sample-eligible events keeps its sample (the 1st, then every Nth),
// bounding ledger growth.
func TestStoreSampleGateOnRetainsSampled(t *testing.T) {
	s, err := NewStore(filepath.Join(t.TempDir(), "savings"))
	if err != nil {
		t.Fatal(err)
	}
	s.SetSampling(true)
	const n = sampleEvery * 2
	for i := 0; i < n; i++ {
		ev := NewEvent(FeatureCheck, "a1", 1000, 100, 0)
		ev.RawSample = "raw-" + string(rune('A'+i))
		ev.KeptSample = "kept"
		if err := s.Record(ev); err != nil {
			t.Fatal(err)
		}
	}
	evs, _ := s.Events(time.Time{})
	retained := 0
	for _, e := range evs {
		if e.RawSample != "" {
			retained++
		}
	}
	if retained != 2 { // events 1 and sampleEvery+1 over 2*sampleEvery records
		t.Errorf("retained %d of %d eligible samples, want 2", retained, n)
	}
	// The very first eligible event must be retained (low-volume installs still
	// capture something) and carry its real bytes.
	if evs[0].RawSample != "raw-A" {
		t.Errorf("first event sample = %q, want raw-A", evs[0].RawSample)
	}
}

// TestStoreReport checks Report fans a single ledger scan into the summary, the
// per-day buckets, and the retained samples together.
func TestStoreReport(t *testing.T) {
	s, err := NewStore(filepath.Join(t.TempDir(), "savings"))
	if err != nil {
		t.Fatal(err)
	}
	s.SetSampling(true)
	a := NewEvent(FeatureCheck, "a1", 1000, 100, 0)
	a.TS = mustDay(t, "2026-06-01T10:00:00Z")
	a.RawSample, a.KeptSample = "raw a", "kept a"
	b := NewEvent(FeatureCommit, "a1", 500, 50, 0)
	b.TS = mustDay(t, "2026-06-02T10:00:00Z")
	if err := s.Record(a); err != nil {
		t.Fatal(err)
	}
	if err := s.Record(b); err != nil {
		t.Fatal(err)
	}
	sum, err := s.Report(time.Time{}, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Events != 2 {
		t.Errorf("events = %d, want 2", sum.Events)
	}
	if len(sum.Buckets) != 2 {
		t.Errorf("buckets = %+v, want 2 days", sum.Buckets)
	}
	if len(sum.Samples) != 1 || sum.Samples[0].RawSample != "raw a" {
		t.Errorf("samples = %+v, want the one retained pair", sum.Samples)
	}
	// Plain Summary (no projections) must omit both.
	plain, _ := s.Summary(time.Time{})
	if plain.Buckets != nil || plain.Samples != nil {
		t.Errorf("plain summary leaked projections: %+v", plain)
	}
}
