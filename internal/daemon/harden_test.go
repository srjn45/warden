package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHardenDataDir(t *testing.T) {
	root := t.TempDir()
	// dataDir + two existing subdirs at 0o755; "inbox" intentionally absent.
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, sub := range []string{"sessions", "closed"} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := HardenDataDir(root); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{root, filepath.Join(root, "sessions"), filepath.Join(root, "closed")} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Fatalf("%s mode = %o, want 700", p, info.Mode().Perm())
		}
	}
	// Absent subdir must not have been created.
	if _, err := os.Stat(filepath.Join(root, "inbox")); !os.IsNotExist(err) {
		t.Fatalf("inbox should not exist, stat err = %v", err)
	}
}
