package metrics

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func ts(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

func TestRecorderRecordAndHistory(t *testing.T) {
	dir := t.TempDir()
	r, err := NewRecorder(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Two samples on day 1, one on day 2.
	must := func(e error) {
		if e != nil {
			t.Fatal(e)
		}
	}
	must(r.Record(Sample{TakenAt: ts("2026-06-08T10:00:00Z"), System: SystemStats{AgentCount: 1}}))
	must(r.Record(Sample{TakenAt: ts("2026-06-08T10:00:15Z"), System: SystemStats{AgentCount: 2}}))
	must(r.Record(Sample{TakenAt: ts("2026-06-09T09:00:00Z"), System: SystemStats{AgentCount: 3}}))

	// History since the second sample → 2 newest-first.
	got, err := r.History(ts("2026-06-08T10:00:15Z"), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].System.AgentCount != 3 || got[1].System.AgentCount != 2 {
		t.Fatalf("history=%+v", got)
	}

	// limit caps the result (newest kept).
	got, _ = r.History(time.Time{}, 1)
	if len(got) != 1 || got[0].System.AgentCount != 3 {
		t.Fatalf("limited history=%+v", got)
	}
}

func TestRecorderPrune(t *testing.T) {
	dir := t.TempDir()
	r, _ := NewRecorder(dir)
	_ = r.Record(Sample{TakenAt: ts("2026-05-01T10:00:00Z")}) // old
	_ = r.Record(Sample{TakenAt: ts("2026-06-09T10:00:00Z")}) // recent
	// keep 7 days relative to now=2026-06-09 → the May file is pruned.
	if err := r.Prune(ts("2026-06-09T12:00:00Z"), 7); err != nil {
		t.Fatal(err)
	}
	got, _ := r.History(time.Time{}, 100)
	if len(got) != 1 || got[0].TakenAt.Day() != 9 {
		t.Fatalf("after prune=%+v", got)
	}
}

func TestRecorderHistoryEmptyDir(t *testing.T) {
	r, _ := NewRecorder(t.TempDir())
	got, err := r.History(time.Time{}, 100)
	if err != nil || len(got) != 0 {
		t.Fatalf("empty history err=%v got=%+v", err, got)
	}
	_ = context.Background()
}

func TestRecorderHistorySkipsMalformedLines(t *testing.T) {
	dir := t.TempDir()
	r, _ := NewRecorder(dir)
	_ = r.Record(Sample{TakenAt: ts("2026-06-09T10:00:00Z"), System: SystemStats{AgentCount: 5}})
	// Append a junk line to the day-file; History must skip it, not error.
	fp := filepath.Join(dir, "2026-06-09.jsonl")
	f, err := os.OpenFile(fp, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("not json\n")
	f.Close()
	got, err := r.History(time.Time{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].System.AgentCount != 5 {
		t.Fatalf("history should skip malformed line: %+v", got)
	}
}
