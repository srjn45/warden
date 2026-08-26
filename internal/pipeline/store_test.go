package pipeline

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestStoreCreateGetList(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	p := &Pipeline{ID: "p1", Name: "p1", Repo: "/r", Status: StatusPending,
		Jobs: []Job{{ID: "a", Prompt: "x", Worktree: "none", Status: JobPending}}}
	if err := s.Create(p); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.Create(p); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate Create want ErrExists, got %v", err)
	}
	got, err := s.Get("p1")
	if err != nil || got.Name != "p1" || len(got.Jobs) != 1 {
		t.Fatalf("Get: %+v err=%v", got, err)
	}
	if _, err := s.Get("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing Get want ErrNotFound, got %v", err)
	}
	all, _ := s.List()
	if len(all) != 1 {
		t.Fatalf("List want 1, got %d", len(all))
	}
}

// TestStoreOwnerIDRoundTrip verifies a delegated pipeline's OwnerID (its
// back-ref to the owning orchestrator, the pipeline-mode analogue of an agent's
// ParentID) survives create → read → list, and that an unset owner stays empty
// (an orchestrator-less human/CLI create).
func TestStoreOwnerIDRoundTrip(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	owned := &Pipeline{ID: "delegated", Name: "delegated", Repo: "/r", Status: StatusPending,
		OwnerID: "orchestrator-1",
		Jobs:    []Job{{ID: "a", Prompt: "x", Worktree: "none", Status: JobPending}}}
	root := &Pipeline{ID: "human", Name: "human", Repo: "/r", Status: StatusPending,
		Jobs: []Job{{ID: "a", Prompt: "x", Worktree: "none", Status: JobPending}}}
	if err := s.Create(owned); err != nil {
		t.Fatalf("Create owned: %v", err)
	}
	if err := s.Create(root); err != nil {
		t.Fatalf("Create root: %v", err)
	}
	gotOwned, err := s.Get("delegated")
	if err != nil || gotOwned.OwnerID != "orchestrator-1" {
		t.Fatalf("Get delegated OwnerID: %+v err=%v", gotOwned, err)
	}
	gotRoot, err := s.Get("human")
	if err != nil || gotRoot.OwnerID != "" {
		t.Fatalf("Get human OwnerID want empty, got %q err=%v", gotRoot.OwnerID, err)
	}
	all, _ := s.List()
	byID := map[string]string{}
	for _, p := range all {
		byID[p.ID] = p.OwnerID
	}
	if byID["delegated"] != "orchestrator-1" || byID["human"] != "" {
		t.Fatalf("List OwnerID mismatch: %v", byID)
	}
}

func TestStoreUpdate(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	s.Create(&Pipeline{ID: "p1", Name: "p1", Repo: "/r", Status: StatusPending,
		Jobs: []Job{{ID: "a", Prompt: "x", Worktree: "none", Status: JobPending}}})
	if err := s.Update("p1", func(p *Pipeline) {
		p.Status = StatusRunning
		p.Job("a").Status = JobDone
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, _ := s.Get("p1")
	if got.Status != StatusRunning || got.Job("a").Status != JobDone {
		t.Fatalf("update not persisted: %+v", got)
	}
}

func TestStoreDelete(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	s.Create(&Pipeline{ID: "p1", Name: "p1", Repo: "/r", Status: StatusDone,
		Jobs: []Job{{ID: "a", Prompt: "x", Worktree: "none", Status: JobDone}}})
	if err := s.Delete("p1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get("p1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after Delete, Get want ErrNotFound, got %v", err)
	}
	if err := s.Delete("p1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete missing want ErrNotFound, got %v", err)
	}
}

// seedLegacy writes a legacy <id>.json pipeline record into dir, the on-disk
// layout the pre-ScrivaDB store used.
func seedLegacy(t *testing.T, dir string, p *Pipeline) {
	t.Helper()
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, p.ID+".json"), data, 0o600); err != nil {
		t.Fatalf("write legacy: %v", err)
	}
}

// TestStoreLegacyImport seeds a dir with legacy <id>.json files, opens a fresh
// Store, and confirms the records are imported (readable via Get/List), the
// sentinel is written, the legacy files are left as a backup, and a second open
// does not re-import.
func TestStoreLegacyImport(t *testing.T) {
	// dir mirrors the daemon's <data>/pipelines; the sentinel lands in its parent.
	root := t.TempDir()
	dir := filepath.Join(root, "pipelines")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	seedLegacy(t, dir, &Pipeline{ID: "p1", Name: "p1", Repo: "/r", Status: StatusDone,
		Jobs: []Job{{ID: "a", Prompt: "x", Worktree: "none", Status: JobDone}}})
	seedLegacy(t, dir, &Pipeline{ID: "p2", Name: "p2", Repo: "/r", Status: StatusRunning,
		Jobs: []Job{{ID: "b", Prompt: "y", Worktree: "none", Status: JobRunning}}})
	// A corrupt file must be skipped, not abort the whole import.
	if err := os.WriteFile(filepath.Join(dir, "bad.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write bad: %v", err)
	}

	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	got, err := s.Get("p1")
	if err != nil || got.Name != "p1" || got.Status != StatusDone {
		t.Fatalf("Get p1 after import: %+v err=%v", got, err)
	}
	all, _ := s.List()
	if len(all) != 2 {
		t.Fatalf("List after import want 2, got %d", len(all))
	}

	sentinel := filepath.Join(root, importedMarker)
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("sentinel missing after import: %v", err)
	}
	// Legacy JSON is retained as a read-only backup.
	if _, err := os.Stat(filepath.Join(dir, "p1.json")); err != nil {
		t.Fatalf("legacy p1.json should be retained: %v", err)
	}
	s.Close()

	// A second open must not re-import: delete a legacy file, drop a new record
	// into the live store, reopen, and confirm the deleted legacy file is NOT
	// resurrected while the live record survives.
	if err := os.Remove(filepath.Join(dir, "p2.json")); err != nil {
		t.Fatalf("rm legacy p2: %v", err)
	}
	s2, err := NewStore(dir)
	if err != nil {
		t.Fatalf("reopen NewStore: %v", err)
	}
	defer s2.Close()
	// p2 was imported into the DB on the first open and stays there (the DB, not
	// the legacy dir, is authoritative now).
	if _, err := s2.Get("p2"); err != nil {
		t.Fatalf("p2 should persist in DB across reopen: %v", err)
	}
	all2, _ := s2.List()
	if len(all2) != 2 {
		t.Fatalf("reopen List want 2 (no re-import), got %d", len(all2))
	}
}

// TestStoreFreshEmptyDir confirms the common test case — a brand-new empty dir
// with no legacy files and no sentinel — works with zero special-casing.
func TestStoreFreshEmptyDir(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer s.Close()
	all, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("fresh List want 0, got %d", len(all))
	}
}
