package spend

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStoreRecordAndTotal(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Fresh store: no spend observed yet.
	if total, err := s.Total(); err != nil || total != 0 {
		t.Fatalf("fresh Total=%d err=%v, want 0/nil", total, err)
	}
	if err := s.Record("a1", "claude", "opus", "/x", 800, 200); err != nil {
		t.Fatal(err)
	}
	if err := s.Record("a2", "claude", "sonnet", "/x", 400, 100); err != nil {
		t.Fatal(err)
	}
	if total, _ := s.Total(); total != 1500 {
		t.Fatalf("Total=%d, want 1500", total)
	}
	// A later, higher cumulative reading for a1 raises its figure on each axis.
	if err := s.Record("a1", "claude", "opus", "/x", 1500, 300); err != nil {
		t.Fatal(err)
	}
	if total, _ := s.Total(); total != 2300 {
		t.Fatalf("Total=%d, want 2300 (1800+500)", total)
	}
}

func TestStoreRecordNeverLowers(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	_ = s.Record("a1", "claude", "opus", "/x", 1500, 500)
	// A momentarily smaller reading (rotated/partial transcript) must not lower the
	// established cumulative figure on either axis.
	if err := s.Record("a1", "claude", "opus", "/x", 700, 200); err != nil {
		t.Fatal(err)
	}
	if total, _ := s.Total(); total != 2000 {
		t.Fatalf("Total=%d, want 2000 (lower reading ignored)", total)
	}
}

func TestStoreRecordIgnoresEmptyAndNonPositive(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	_ = s.Record("", "claude", "opus", "/x", 1000, 0)  // no session id
	_ = s.Record("a1", "claude", "opus", "/x", 0, 0)   // unmeasurable on both axes
	_ = s.Record("a1", "claude", "opus", "/x", -5, -5) // never
	if total, _ := s.Total(); total != 0 {
		t.Fatalf("Total=%d, want 0 (no valid records)", total)
	}
}

func TestStoreSessionsStampsModelRepoDay(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	_ = s.Record("a1", "claude", "opus", "/x", 800, 200)
	sessions, err := s.Sessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("want 1 session, got %d", len(sessions))
	}
	e := sessions[0]
	if e.Session != "a1" || e.Backend != "claude" || e.Model != "opus" || e.Repo != "/x" || e.Input != 800 || e.Output != 200 {
		t.Fatalf("session not stamped correctly: %+v", e)
	}
	if e.Day == "" {
		t.Errorf("expected a first-seen day to be stamped")
	}
}

func TestStorePersistsAcrossInstances(t *testing.T) {
	dir := t.TempDir()
	s1, _ := NewStore(dir)
	_ = s1.Record("a1", "claude", "opus", "/x", 1000, 234)
	// A second store rooted at the same dir reads the persisted figure.
	s2, _ := NewStore(dir)
	if total, _ := s2.Total(); total != 1234 {
		t.Fatalf("reloaded Total=%d, want 1234", total)
	}
}

func TestStoreToleratesCorruptFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "spend.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, _ := NewStore(dir)
	// A corrupt file degrades to empty rather than erroring, and a fresh Record
	// recovers the gauge.
	if total, err := s.Total(); err != nil || total != 0 {
		t.Fatalf("corrupt Total=%d err=%v, want 0/nil", total, err)
	}
	if err := s.Record("a1", "claude", "opus", "/x", 42, 0); err != nil {
		t.Fatal(err)
	}
	if total, _ := s.Total(); total != 42 {
		t.Fatalf("Total=%d, want 42 after recovery", total)
	}
}
