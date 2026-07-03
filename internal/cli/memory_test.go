package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/srjn45/warden/internal/memory"
)

// runMemory drives `wd memory` with the store pinned to a temp repo root (no real
// git repo, no daemon) and returns combined stdout+stderr. It restores the injected
// package vars on cleanup.
func runMemory(t *testing.T, root string, args ...string) (string, error) {
	t.Helper()
	prev := memoryStore
	memoryStore = func() *memory.Store {
		return &memory.Store{RepoRoot: func(context.Context, string) (string, error) { return root, nil }}
	}
	t.Cleanup(func() { memoryStore = prev })

	cmd := newRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	full := append([]string{"memory"}, args...)
	full = append(full, "--config", t.TempDir()+"/none.yaml")
	cmd.SetArgs(full)
	err := cmd.Execute()
	return out.String(), err
}

// TestMemoryShowAutoCreates: default `wd memory` resolves + auto-creates the file,
// prints its path with the (auto-created) marker, and shows the empty-state hint for
// a freshly seeded (entry-less) memory.
func TestMemoryShowAutoCreates(t *testing.T) {
	root := t.TempDir()
	out, err := runMemory(t, root)
	if err != nil {
		t.Fatalf("wd memory: %v", err)
	}
	if !strings.Contains(out, filepath.Join(root, ".warden", "memory.md")) {
		t.Errorf("output missing resolved path:\n%s", out)
	}
	if !strings.Contains(out, "auto-created") {
		t.Errorf("first show missing auto-created marker:\n%s", out)
	}
	if !strings.Contains(out, "no memory entries yet") {
		t.Errorf("empty memory missing hint:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(root, ".warden", "memory.md")); err != nil {
		t.Errorf("file not created: %v", err)
	}
}

// TestMemoryShowRendersEntries: with hand-authored entries, default show prints the
// rendered projection (header + bullets + unverified caveat).
func TestMemoryShowRendersEntries(t *testing.T) {
	root := t.TempDir()
	writeMemory(t, root, "- [trusted · 2026-07-01] daemon API is spec-first\n- [unverified · 2026-07-01] cache in ~/.warden\n")
	out, err := runMemory(t, root)
	if err != nil {
		t.Fatalf("wd memory: %v", err)
	}
	if !strings.Contains(out, "daemon API is spec-first") {
		t.Errorf("missing trusted entry:\n%s", out)
	}
	if !strings.Contains(out, "may be stale, verify before relying") {
		t.Errorf("missing unverified caveat:\n%s", out)
	}
}

// TestMemoryRaw prints the file verbatim (un-rendered) — the raw bytes, not the
// projection.
func TestMemoryRaw(t *testing.T) {
	root := t.TempDir()
	writeMemory(t, root, "verbatim line\n- a bullet\n")
	out, err := runMemory(t, root, "--raw")
	if err != nil {
		t.Fatalf("wd memory --raw: %v", err)
	}
	if !strings.Contains(out, "verbatim line") {
		t.Errorf("--raw dropped non-bullet content:\n%s", out)
	}
	if strings.Contains(out, "warden project memory (durable") {
		t.Errorf("--raw must not render the projection header:\n%s", out)
	}
}

// TestMemoryPath prints just the resolved path and does NOT auto-create the file.
func TestMemoryPath(t *testing.T) {
	root := t.TempDir()
	out, err := runMemory(t, root, "--path")
	if err != nil {
		t.Fatalf("wd memory --path: %v", err)
	}
	want := filepath.Join(root, ".warden", "memory.md")
	if strings.TrimSpace(out) != want {
		t.Errorf("--path = %q, want %q", strings.TrimSpace(out), want)
	}
	if _, err := os.Stat(want); !os.IsNotExist(err) {
		t.Errorf("--path must not create the file; stat err = %v", err)
	}
}

// TestMemoryEditResolvesEditor: --edit auto-creates the file then hands it to the
// resolved editor. We stub openEditor to capture the path instead of spawning one.
func TestMemoryEditResolvesEditor(t *testing.T) {
	root := t.TempDir()
	var got string
	prev := openEditor
	openEditor = func(_ *cobra.Command, path string) error { got = path; return nil }
	t.Cleanup(func() { openEditor = prev })

	if _, err := runMemory(t, root, "--edit"); err != nil {
		t.Fatalf("wd memory --edit: %v", err)
	}
	want := filepath.Join(root, ".warden", "memory.md")
	if got != want {
		t.Errorf("editor opened %q, want %q", got, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Errorf("--edit must auto-create first: %v", err)
	}
}

// writeMemory seeds .warden/memory.md under root with content (creating .warden).
func writeMemory(t *testing.T, root, content string) {
	t.Helper()
	dir := filepath.Join(root, ".warden")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "memory.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
