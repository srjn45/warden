package insights

import (
	"testing"
	"time"
)

func TestSuggestParallelizationDisjointSequential(t *testing.T) {
	// Two finished sessions, same repo, back-to-back (no overlap), disjoint files.
	sessions := []SessionRecord{
		rec("s1", "code", "done", "/r", 0, 20, "a.go", "a_test.go"),
		rec("s2", "code", "done", "/r", 30, 10, "b.go"),
	}
	got := SuggestParallelization(sessions, base)
	if len(got) != 1 {
		t.Fatalf("disjoint sequential pair must be suggested, got %d", len(got))
	}
	s := got[0]
	if s.A != "s1" || s.B != "s2" {
		t.Fatalf("suggestion ids wrong: %s,%s", s.A, s.B)
	}
	// Saving is the shorter run (s2 = 10m = 600s).
	if s.SavedSec != 600 {
		t.Fatalf("saved=%d, want 600 (shorter run)", s.SavedSec)
	}
	if s.Repo != "/r" {
		t.Fatalf("repo=%q", s.Repo)
	}
}

func TestSuggestParallelizationSharedFileNotSuggested(t *testing.T) {
	// Overlapping file set ⇒ possible dependency ⇒ NOT suggested.
	sessions := []SessionRecord{
		rec("s1", "code", "done", "/r", 0, 20, "shared.go", "a.go"),
		rec("s2", "code", "done", "/r", 30, 10, "shared.go", "b.go"),
	}
	if got := SuggestParallelization(sessions, base); len(got) != 0 {
		t.Fatalf("sessions sharing a file must NOT be suggested, got %+v", got)
	}
}

func TestSuggestParallelizationAlreadyOverlappingNotSuggested(t *testing.T) {
	// Windows overlap (already ran concurrently) ⇒ nothing to suggest.
	sessions := []SessionRecord{
		rec("s1", "code", "done", "/r", 0, 30, "a.go"),
		rec("s2", "code", "done", "/r", 10, 10, "b.go"), // starts inside s1's window
	}
	if got := SuggestParallelization(sessions, base); len(got) != 0 {
		t.Fatalf("temporally overlapping sessions must NOT be suggested, got %+v", got)
	}
}

func TestSuggestParallelizationDifferentRepoNotSuggested(t *testing.T) {
	sessions := []SessionRecord{
		rec("s1", "code", "done", "/r1", 0, 20, "a.go"),
		rec("s2", "code", "done", "/r2", 30, 10, "b.go"),
	}
	if got := SuggestParallelization(sessions, base); len(got) != 0 {
		t.Fatalf("cross-repo pair must NOT be suggested, got %+v", got)
	}
}

func TestSuggestParallelizationSkipsActiveAndFileless(t *testing.T) {
	sessions := []SessionRecord{
		{ID: "live", Type: "code", Status: "working", Repo: "/r", Start: base, Files: []string{"a.go"}}, // active
		rec("nofiles", "code", "done", "/r", 30, 10),                                                    // no files
	}
	if got := SuggestParallelization(sessions, base.Add(time.Hour)); len(got) != 0 {
		t.Fatalf("active/fileless sessions are not comparable, got %+v", got)
	}
}

func TestSuggestParallelizationSortedBySaving(t *testing.T) {
	// Three mutually-disjoint, sequential sessions in one repo: 3 pairs.
	sessions := []SessionRecord{
		rec("s1", "code", "done", "/r", 0, 5, "a.go"),   // shortest
		rec("s2", "code", "done", "/r", 10, 30, "b.go"), // longest
		rec("s3", "code", "done", "/r", 50, 20, "c.go"),
	}
	got := SuggestParallelization(sessions, base)
	if len(got) != 3 {
		t.Fatalf("want 3 pairs, got %d", len(got))
	}
	// Each pair's saving = shorter run. (s2,s3)=20m, (s2,s1)=5m, (s3,s1)=5m.
	// Sorted by saving desc then ids: (s2,s3) first.
	if got[0].A != "s2" || got[0].B != "s3" || got[0].SavedSec != 1200 {
		t.Fatalf("top suggestion should be s2/s3 @1200s, got %+v", got[0])
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].SavedSec < got[i].SavedSec {
			t.Fatalf("suggestions not sorted by saving desc: %+v", got)
		}
	}
}
