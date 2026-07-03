package memory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubRoot returns a Store whose repo-root resolver always points at root — so
// locate/resolve tests need no real git repo.
func stubRoot(root string) *Store {
	return &Store{RepoRoot: func(context.Context, string) (string, error) { return root, nil }}
}

// TestLocateKeysOffRepoRoot: Locate joins the repo root with .warden/memory.md and
// does NOT create anything (it's a pure path resolve).
func TestLocateKeysOffRepoRoot(t *testing.T) {
	root := t.TempDir()
	s := stubRoot(root)
	path, err := s.Locate(context.Background(), filepath.Join(root, "some", "subdir"))
	if err != nil {
		t.Fatalf("Locate: %v", err)
	}
	want := filepath.Join(root, ".warden", "memory.md")
	if path != want {
		t.Fatalf("Locate = %q, want %q", path, want)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("Locate must not create the file; stat err = %v", err)
	}
}

// TestResolveAutoCreatesOnFirstUse: the first Resolve seeds .warden/memory.md (and
// its parent dir) with the header template and reports created=true; the second
// Resolve finds it and reports created=false, leaving content untouched.
func TestResolveAutoCreatesOnFirstUse(t *testing.T) {
	root := t.TempDir()
	s := stubRoot(root)

	path, created, err := s.Resolve(context.Background(), root)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !created {
		t.Fatalf("first Resolve created = false, want true")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read seeded file: %v", err)
	}
	if !strings.Contains(string(data), "warden project memory") {
		t.Fatalf("seeded file missing header template:\n%s", data)
	}

	// Mutate, then re-resolve: must not clobber.
	if err := os.WriteFile(path, []byte("- a human fact\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, created2, err := s.Resolve(context.Background(), root)
	if err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	if created2 {
		t.Fatalf("second Resolve created = true, want false")
	}
	data2, _ := os.ReadFile(path)
	if string(data2) != "- a human fact\n" {
		t.Fatalf("Resolve clobbered existing content: %q", data2)
	}
}

// TestLoadParsesResolvedFile: Load auto-creates then parses; a freshly seeded file is
// header-only (preamble present, no entries).
func TestLoadParsesResolvedFile(t *testing.T) {
	root := t.TempDir()
	s := stubRoot(root)
	m, _, created, err := s.Load(context.Background(), root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !created {
		t.Fatalf("Load created = false on first use")
	}
	if len(m.Entries) != 0 {
		t.Fatalf("freshly seeded memory has %d entries, want 0", len(m.Entries))
	}
	if !strings.Contains(m.Preamble, "warden project memory") {
		t.Fatalf("preamble missing header:\n%s", m.Preamble)
	}
}
