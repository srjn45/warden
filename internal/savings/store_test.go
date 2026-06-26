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
