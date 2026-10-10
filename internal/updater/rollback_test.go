package updater

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/schema"
)

func TestRollbackNoSchemaChange(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	binPath := filepath.Join(dir, "warden")
	bakPath := binPath + ".bak"

	require.NoError(t, os.WriteFile(binPath, []byte("new-bin"), 0o755))
	require.NoError(t, os.WriteFile(bakPath, []byte("old-bin"), 0o755))

	// Data is at schema 1
	require.NoError(t, os.MkdirAll(filepath.Join(dataDir, "backends"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "backends", "backends.scriva"), []byte("store-v1"), 0o600))
	l := &schema.Ledger{
		SchemaVersion: 1,
		BinaryVersion: "1.0.0",
		History:       []schema.HistoryEntry{{From: 0, To: 1, Migration: "baseline-fresh"}},
	}
	require.NoError(t, schema.Save(dataDir, l))

	// Snapshot is also at schema 1 (no schema change occurred during update)
	snapDir, err := SnapshotStores(dataDir, "1.0.0", binPath)
	require.NoError(t, err)

	svc := &fakeSvc{}
	clk := newFakeClock()
	out := &bytes.Buffer{}
	probe := &fakeProbe{fn: func() (Health, error) {
		return Health{Status: "ok", Version: "1.0.0", SchemaVersion: 1}, nil
	}}

	confirmedCalled := false
	opts := RollbackOptions{
		DataDir:      dataDir,
		InstallBin:   binPath,
		ReadyTimeout: 5 * time.Second,
		Service:      svc,
		Prober:       probe,
		Installer:    fsInstaller{bin: binPath},
		Clock:        clk,
		Stdout:       out,
		Confirm: func(string) bool {
			confirmedCalled = true
			return true
		},
	}

	res, err := Rollback(opts)
	require.NoError(t, err)
	require.False(t, confirmedCalled, "no confirmation prompt needed when schema did not change")
	require.False(t, res.SchemaChanged)
	require.False(t, res.DataRestored)
	require.Contains(t, res.Message, "no schema change")

	// Binary was restored from bakPath
	b, err := os.ReadFile(binPath)
	require.NoError(t, err)
	require.Equal(t, "old-bin", string(b))

	// Service was stopped and restarted
	require.Equal(t, 1, svc.stops)
	require.Equal(t, 1, svc.restarts)
	_ = snapDir
}

func TestRollbackSchemaChangeRequiresConfirmationDeclined(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	binPath := filepath.Join(dir, "warden")
	bakPath := binPath + ".bak"

	require.NoError(t, os.WriteFile(binPath, []byte("new-bin"), 0o755))
	require.NoError(t, os.WriteFile(bakPath, []byte("old-bin"), 0o755))

	// Snapshot was created when schema was 1
	l1 := &schema.Ledger{
		SchemaVersion: 1,
		BinaryVersion: "1.0.0",
		History:       []schema.HistoryEntry{{From: 0, To: 1, Migration: "baseline-fresh"}},
	}
	require.NoError(t, schema.Save(dataDir, l1))
	require.NoError(t, os.MkdirAll(filepath.Join(dataDir, "backends"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "backends", "backends.scriva"), []byte("store-v1"), 0o600))
	snapDir, err := SnapshotStores(dataDir, "1.0.0", binPath)
	require.NoError(t, err)

	// Now current data advanced to schema 2
	l2 := &schema.Ledger{
		SchemaVersion: 2,
		BinaryVersion: "2.0.0",
		History:       []schema.HistoryEntry{{From: 0, To: 1, Migration: "baseline-fresh"}, {From: 1, To: 2, Migration: "v2-mig"}},
	}
	require.NoError(t, schema.Save(dataDir, l2))
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "backends", "backends.scriva"), []byte("store-v2"), 0o600))

	svc := &fakeSvc{}
	clk := newFakeClock()
	out := &bytes.Buffer{}
	probe := &fakeProbe{fn: func() (Health, error) {
		return Health{Status: "ok", Version: "2.0.0", SchemaVersion: 2}, nil
	}}

	confirmPromptSeen := ""
	opts := RollbackOptions{
		DataDir:      dataDir,
		InstallBin:   binPath,
		ReadyTimeout: 5 * time.Second,
		Service:      svc,
		Prober:       probe,
		Installer:    fsInstaller{bin: binPath},
		Clock:        clk,
		Stdout:       out,
		Confirm: func(msg string) bool {
			confirmPromptSeen = msg
			return false // decline confirmation
		},
	}

	_, err = Rollback(opts)
	require.Error(t, err)
	require.Contains(t, err.Error(), "rollback cancelled")
	require.Contains(t, confirmPromptSeen, "schema v2 -> v1")
	require.Contains(t, confirmPromptSeen, "Any changes made since the update will be lost")

	// Binary was not changed
	b, err := os.ReadFile(binPath)
	require.NoError(t, err)
	require.Equal(t, "new-bin", string(b))

	// Data was not touched
	currStore, err := os.ReadFile(filepath.Join(dataDir, "backends", "backends.scriva"))
	require.NoError(t, err)
	require.Equal(t, "store-v2", string(currStore))
	_ = snapDir
}

func TestRollbackSchemaChangeConfirmed(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	binPath := filepath.Join(dir, "warden")
	bakPath := binPath + ".bak"

	require.NoError(t, os.WriteFile(binPath, []byte("new-bin"), 0o755))
	require.NoError(t, os.WriteFile(bakPath, []byte("old-bin"), 0o755))

	// Snapshot was created at schema 1
	l1 := &schema.Ledger{
		SchemaVersion: 1,
		BinaryVersion: "1.0.0",
		History:       []schema.HistoryEntry{{From: 0, To: 1, Migration: "baseline-fresh"}},
	}
	require.NoError(t, schema.Save(dataDir, l1))
	require.NoError(t, os.MkdirAll(filepath.Join(dataDir, "backends"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "backends", "backends.scriva"), []byte("store-v1"), 0o600))
	snapDir, err := SnapshotStores(dataDir, "1.0.0", binPath)
	require.NoError(t, err)

	// Live data advanced to schema 2
	l2 := &schema.Ledger{
		SchemaVersion: 2,
		BinaryVersion: "2.0.0",
		History:       []schema.HistoryEntry{{From: 0, To: 1, Migration: "baseline-fresh"}, {From: 1, To: 2, Migration: "v2-mig"}},
	}
	require.NoError(t, schema.Save(dataDir, l2))
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "backends", "backends.scriva"), []byte("store-v2-modified"), 0o600))

	svc := &fakeSvc{}
	clk := newFakeClock()
	out := &bytes.Buffer{}
	probe := &fakeProbe{fn: func() (Health, error) {
		if svc.restarts == 0 {
			return Health{Status: "ok", Version: "2.0.0", SchemaVersion: 2}, nil
		}
		return Health{Status: "ok", Version: "1.0.0", SchemaVersion: 1}, nil
	}}

	opts := RollbackOptions{
		DataDir:      dataDir,
		InstallBin:   binPath,
		ReadyTimeout: 5 * time.Second,
		Force:        true, // skip confirmation
		Service:      svc,
		Prober:       probe,
		Installer:    fsInstaller{bin: binPath},
		Clock:        clk,
		Stdout:       out,
	}

	res, err := Rollback(opts)
	require.NoError(t, err)
	require.True(t, res.SchemaChanged)
	require.True(t, res.DataRestored)
	require.Contains(t, res.Message, "schema v1")

	// Binary restored to old
	b, err := os.ReadFile(binPath)
	require.NoError(t, err)
	require.Equal(t, "old-bin", string(b))

	// Data restored to v1
	currStore, err := os.ReadFile(filepath.Join(dataDir, "backends", "backends.scriva"))
	require.NoError(t, err)
	require.Equal(t, "store-v1", string(currStore))

	// Ledger restored to v1
	restoredLedger, err := schema.Load(dataDir)
	require.NoError(t, err)
	require.Equal(t, 1, restoredLedger.SchemaVersion)
	require.Nil(t, restoredLedger.InProgress)
	_ = snapDir
}
