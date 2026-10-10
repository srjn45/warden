package updater

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/schema"
)

func TestSnapshotStoresAndRestore(t *testing.T) {
	dataDir := t.TempDir()
	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "warden")
	require.NoError(t, os.WriteFile(binPath, []byte("bin-content"), 0o755))

	// Create initial schema ledger
	l := &schema.Ledger{
		SchemaVersion: 1,
		BinaryVersion: "1.0.0",
		History:       []schema.HistoryEntry{{From: 0, To: 1, Migration: "baseline-fresh"}},
	}
	require.NoError(t, schema.Save(dataDir, l))

	// Create stores
	require.NoError(t, os.MkdirAll(filepath.Join(dataDir, "backends"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "backends", "backends.scriva"), []byte("store-content-1"), 0o600))

	require.NoError(t, os.MkdirAll(filepath.Join(dataDir, "snapshots-db"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "snapshots-db", "meta.json"), []byte("meta-content"), 0o600))

	// Create excluded directories and files
	require.NoError(t, os.MkdirAll(filepath.Join(dataDir, "snapshots"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "snapshots", "flat_transcript.txt"), []byte("flat-transcript"), 0o600))

	require.NoError(t, os.MkdirAll(filepath.Join(dataDir, "transcripts"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "transcripts", "agent.log"), []byte("transcript-log"), 0o600))

	require.NoError(t, os.MkdirAll(filepath.Join(dataDir, "tmp"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "tmp", "temp.file"), []byte("temp"), 0o600))

	require.NoError(t, os.WriteFile(filepath.Join(dataDir, ".owner.lock"), []byte("lock"), 0o600))

	// Take snapshot
	snapDir, err := SnapshotStores(dataDir, "1.0.0", binPath)
	require.NoError(t, err)
	require.DirExists(t, snapDir)

	// Verify manifest
	require.FileExists(t, filepath.Join(snapDir, "manifest.json"))

	// Verify snapshotted files
	require.FileExists(t, filepath.Join(snapDir, "backends", "backends.scriva"))
	b1, err := os.ReadFile(filepath.Join(snapDir, "backends", "backends.scriva"))
	require.NoError(t, err)
	require.Equal(t, "store-content-1", string(b1))

	require.FileExists(t, filepath.Join(snapDir, "snapshots-db", "meta.json"))
	require.FileExists(t, filepath.Join(snapDir, "schema.json"))
	require.FileExists(t, filepath.Join(snapDir, "bin", "warden"))

	// Verify excluded files are NOT in snapshot
	require.NoFileExists(t, filepath.Join(snapDir, "snapshots", "flat_transcript.txt"))
	require.NoFileExists(t, filepath.Join(snapDir, "transcripts", "agent.log"))
	require.NoFileExists(t, filepath.Join(snapDir, "tmp", "temp.file"))
	require.NoFileExists(t, filepath.Join(snapDir, ".owner.lock"))

	// Simulate corruption/modification in live dataDir
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "backends", "backends.scriva"), []byte("corrupted-content"), 0o600))
	l.InProgress = &schema.InProgress{Migration: "mig-1", Step: "step-1"}
	require.NoError(t, schema.Save(dataDir, l))

	// Restore snapshot
	require.NoError(t, RestoreSnapshot(snapDir, dataDir))

	// Verify content was restored
	bRestored, err := os.ReadFile(filepath.Join(dataDir, "backends", "backends.scriva"))
	require.NoError(t, err)
	require.Equal(t, "store-content-1", string(bRestored))

	// Verify InProgress is cleared on restored ledger
	lRestored, err := schema.Load(dataDir)
	require.NoError(t, err)
	require.Nil(t, lRestored.InProgress)
	require.Equal(t, 1, lRestored.SchemaVersion)
}

func TestPruneSnapshotsRetention(t *testing.T) {
	dataDir := t.TempDir()
	backupsDir := filepath.Join(dataDir, "backups")
	require.NoError(t, os.MkdirAll(backupsDir, 0o700))

	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	clk := &fakeClock{now: now}

	// Helper to make dummy snapshot
	makeSnap := func(name string, modTime time.Time) {
		p := filepath.Join(backupsDir, name)
		require.NoError(t, os.MkdirAll(p, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(p, "manifest.json"), []byte("{}"), 0o600))
		require.NoError(t, os.Chtimes(p, modTime, modTime))
	}

	// 5 snapshots, all 30 days old
	t1 := now.Add(-30 * 24 * time.Hour)
	makeSnap("pre-v1.0.0-20260910T120000Z", t1)
	makeSnap("pre-v1.0.1-20260910T130000Z", t1.Add(time.Hour))
	makeSnap("pre-v1.0.2-20260910T140000Z", t1.Add(2*time.Hour))
	makeSnap("pre-v1.0.3-20260910T150000Z", t1.Add(3*time.Hour))
	makeSnap("pre-v1.0.4-20260910T160000Z", t1.Add(4*time.Hour))

	// Retention: keep last 2 or 14 days, whichever keeps more.
	// Since all are 30 days old (0 within 14 days), last 2 keeps 2.
	pruned, err := PruneSnapshots(dataDir, clk, 2, 14*24*time.Hour)
	require.NoError(t, err)
	require.Len(t, pruned, 3)

	// Verify the 2 newest survive
	require.DirExists(t, filepath.Join(backupsDir, "pre-v1.0.4-20260910T160000Z"))
	require.DirExists(t, filepath.Join(backupsDir, "pre-v1.0.3-20260910T150000Z"))
	require.NoDirExists(t, filepath.Join(backupsDir, "pre-v1.0.0-20260910T120000Z"))

	// Now add 3 snapshots within 14 days (e.g. 2 days ago)
	tRecent := now.Add(-2 * 24 * time.Hour)
	makeSnap("pre-v1.1.0-20261008T120000Z", tRecent)
	makeSnap("pre-v1.1.1-20261008T130000Z", tRecent.Add(time.Hour))
	makeSnap("pre-v1.1.2-20261008T140000Z", tRecent.Add(2*time.Hour))

	// Now we have: 2 old (from above) and 3 recent.
	// 14 days window includes the 3 recent snapshots (> 2).
	// So all 3 recent should be kept, and the 2 30-day-old ones should be pruned.
	pruned2, err := PruneSnapshots(dataDir, clk, 2, 14*24*time.Hour)
	require.NoError(t, err)
	require.Len(t, pruned2, 2)
	require.DirExists(t, filepath.Join(backupsDir, "pre-v1.1.0-20261008T120000Z"))
	require.DirExists(t, filepath.Join(backupsDir, "pre-v1.1.1-20261008T130000Z"))
	require.DirExists(t, filepath.Join(backupsDir, "pre-v1.1.2-20261008T140000Z"))
}
