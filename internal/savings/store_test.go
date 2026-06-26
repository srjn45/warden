package savings

import (
	"path/filepath"
	"testing"
	"time"
)

func TestStoreRecordAndEvents(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(filepath.Join(dir, "savings"))
	if err != nil {
		t.Fatal(err)
	}

	// Empty ledger reads as no events, not an error.
	evs, err := s.Events(time.Time{})
	if err != nil {
		t.Fatalf("Events on fresh store: %v", err)
	}
	if len(evs) != 0 {
		t.Fatalf("fresh store has %d events, want 0", len(evs))
	}

	if err := s.Record(NewEvent(FeatureCheck, "a1", 1000, 100, 0)); err != nil {
		t.Fatal(err)
	}
	if err := s.Record(NewEvent(FeatureCommit, "a2", 200, 20, 0)); err != nil {
		t.Fatal(err)
	}

	evs, err = s.Events(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 {
		t.Fatalf("got %d events, want 2", len(evs))
	}
	if evs[0].Feature != FeatureCheck || evs[0].Saved != 900 {
		t.Errorf("evs[0] = %+v", evs[0])
	}
}

func TestStoreEventsSinceFilter(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(filepath.Join(dir, "savings"))
	if err != nil {
		t.Fatal(err)
	}
	old := Event{TS: time.Now().Add(-48 * time.Hour).UTC(), Feature: FeatureCheck, RawTokens: 100, Saved: 100}
	recent := Event{TS: time.Now().Add(-1 * time.Hour).UTC(), Feature: FeatureCheck, RawTokens: 200, Saved: 200}
	if err := s.Record(old); err != nil {
		t.Fatal(err)
	}
	if err := s.Record(recent); err != nil {
		t.Fatal(err)
	}

	since := time.Now().Add(-24 * time.Hour)
	evs, err := s.Events(since)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Saved != 200 {
		t.Fatalf("since-filter got %+v, want only the recent event", evs)
	}
}

func TestStoreCalibrationSamples(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(filepath.Join(dir, "savings"))
	if err != nil {
		t.Fatal(err)
	}
	s.SetSampling(true) // retention is off by default; calibration needs real bytes

	// First sample-eligible event is always retained (1-in-sampleEvery keeps the 1st).
	ev := NewEvent(FeatureCheck, "a1", 1000, 100, 0)
	ev.RawSample = "raw command output bytes"
	ev.KeptSample = "kept"
	if err := s.Record(ev); err != nil {
		t.Fatal(err)
	}
	// An event with no samples contributes nothing.
	if err := s.Record(NewEvent(FeatureCommit, "a2", 200, 20, 0)); err != nil {
		t.Fatal(err)
	}

	got, err := s.CalibrationSamples(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	// Both the raw and kept side of the one retained event surface.
	if len(got) != 2 {
		t.Fatalf("CalibrationSamples = %v, want 2 strings", got)
	}
	if got[0] != "raw command output bytes" || got[1] != "kept" {
		t.Fatalf("CalibrationSamples = %v, want raw then kept", got)
	}

	// Calibration() reads the sidecar that lives next to the ledger.
	if _, ok, err := s.Calibration(); err != nil || ok {
		t.Fatalf("fresh store Calibration: ok=%v err=%v, want ok=false", ok, err)
	}
	if err := SaveCalibration(filepath.Join(dir, "savings"), Calibration{BytesPerToken: 3.5, Samples: 2, Model: CalibrationModel}); err != nil {
		t.Fatal(err)
	}
	cal, ok, err := s.Calibration()
	if err != nil || !ok {
		t.Fatalf("Calibration after save: ok=%v err=%v", ok, err)
	}
	if cal.BytesPerToken != 3.5 {
		t.Fatalf("Calibration factor = %v, want 3.5", cal.BytesPerToken)
	}
}

func TestStoreSummary(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(filepath.Join(dir, "savings"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Record(NewEvent(FeatureCheck, "a1", 1000, 0, 0)); err != nil {
		t.Fatal(err)
	}
	sum, err := s.Summary(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if sum.SavedTokens != 1000 || sum.Events != 1 {
		t.Errorf("Summary = %+v", sum)
	}
}
