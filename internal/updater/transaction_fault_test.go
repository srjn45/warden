package updater

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/schema"
)

func TestTxnFaultKillMidMigration(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	binPath := filepath.Join(dir, "warden")
	stagedPath := filepath.Join(dir, "new")

	require.NoError(t, os.WriteFile(binPath, []byte("old-bin"), 0o755))
	require.NoError(t, os.WriteFile(stagedPath, []byte("new-bin"), 0o755))

	// Setup dataDir with store and schema ledger v1
	require.NoError(t, os.MkdirAll(filepath.Join(dataDir, "backends"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "backends", "backends.scriva"), []byte("pristine-v1-store"), 0o600))
	l := &schema.Ledger{
		SchemaVersion: 1,
		BinaryVersion: "1.0.0",
		History:       []schema.HistoryEntry{{From: 0, To: 1, Migration: "baseline-fresh"}},
	}
	require.NoError(t, schema.Save(dataDir, l))

	svc := &fakeSvc{}
	clk := newFakeClock()
	out := &bytes.Buffer{}
	probe := &fakeProbe{fn: func() (Health, error) {
		return Health{Status: "ok", Version: "1.0.0", SchemaVersion: 1}, nil
	}}

	opts := Options{
		CurrentVersion: "1.0.0",
		TargetVersion:  "2.0.0",
		InstallBin:     binPath,
		DataDir:        dataDir,
		Stdout:         out,
		ReadyTimeout:   10 * time.Second,
		Migrate: func() error {
			// Simulate being killed mid-migration after corrupting data
			_ = os.WriteFile(filepath.Join(dataDir, "backends", "backends.scriva"), []byte("partial-migration-damage"), 0o600)
			led, _ := schema.Load(dataDir)
			if led != nil {
				led.InProgress = &schema.InProgress{Migration: "v2-migration", Step: "partial"}
				_ = schema.Save(dataDir, led)
			}
			return errors.New("killed mid-migration: SIGKILL / unhandled exception")
		},
	}

	tx := &txn{
		opts:   opts,
		svc:    svc,
		probe:  probe,
		inst:   fsInstaller{bin: binPath},
		clock:  clk,
		target: "2.0.0",
	}

	tx.capture(context.Background())
	rb, err := tx.apply(context.Background(), stagedPath)

	require.Error(t, err)
	require.True(t, rb, "rollback must report successful")

	var fe *FailureError
	require.ErrorAs(t, err, &fe)
	require.NoError(t, fe.Rollback, "rollback itself succeeded without error")
	require.Contains(t, err.Error(), "killed mid-migration")
	require.Contains(t, err.Error(), "restored v1.0.0")

	// Binary was restored
	binContent, err := os.ReadFile(binPath)
	require.NoError(t, err)
	require.Equal(t, "old-bin", string(binContent))

	// Data was restored from snapshot
	storeContent, err := os.ReadFile(filepath.Join(dataDir, "backends", "backends.scriva"))
	require.NoError(t, err)
	require.Equal(t, "pristine-v1-store", string(storeContent))

	// Restored ledger has InProgress cleared
	restoredLedger, err := schema.Load(dataDir)
	require.NoError(t, err)
	require.Nil(t, restoredLedger.InProgress)
	require.Equal(t, 1, restoredLedger.SchemaVersion)

	// Service was restarted back
	require.Equal(t, 1, svc.stops)
	require.Equal(t, 1, svc.restarts)
}

func TestTxnFaultDiskFull(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	binPath := filepath.Join(dir, "warden")
	stagedPath := filepath.Join(dir, "new")

	require.NoError(t, os.WriteFile(binPath, []byte("old-bin"), 0o755))
	require.NoError(t, os.WriteFile(stagedPath, []byte("new-bin"), 0o755))

	// Create backups dir as read-only to simulate failure creating snapshot (e.g. disk full / EACCES)
	require.NoError(t, os.MkdirAll(dataDir, 0o700))
	backupsDir := filepath.Join(dataDir, "backups")
	require.NoError(t, os.MkdirAll(backupsDir, 0o500))
	defer os.Chmod(backupsDir, 0o700)

	svc := &fakeSvc{}
	clk := newFakeClock()
	out := &bytes.Buffer{}
	probe := &fakeProbe{fn: func() (Health, error) {
		return Health{Status: "ok", Version: "1.0.0"}, nil
	}}

	opts := Options{
		CurrentVersion: "1.0.0",
		TargetVersion:  "2.0.0",
		InstallBin:     binPath,
		DataDir:        dataDir,
		Stdout:         out,
		ReadyTimeout:   10 * time.Second,
	}

	tx := &txn{
		opts:   opts,
		svc:    svc,
		probe:  probe,
		inst:   fsInstaller{bin: binPath},
		clock:  clk,
		target: "2.0.0",
	}

	tx.capture(context.Background())
	rb, err := tx.apply(context.Background(), stagedPath)

	require.Error(t, err)
	require.False(t, rb, "no swap happened, so no rollback needed")
	require.Contains(t, err.Error(), "snapshot data")

	// Binary was never modified
	binContent, err := os.ReadFile(binPath)
	require.NoError(t, err)
	require.Equal(t, "old-bin", string(binContent))

	// Service was restarted back after snapshot failed
	require.Equal(t, 1, svc.stops)
	require.Equal(t, 1, svc.restarts)
}

func TestTxnFaultHealthFailure(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	binPath := filepath.Join(dir, "warden")
	stagedPath := filepath.Join(dir, "new")

	require.NoError(t, os.WriteFile(binPath, []byte("old-bin"), 0o755))
	require.NoError(t, os.WriteFile(stagedPath, []byte("new-bin"), 0o755))

	// Setup dataDir
	require.NoError(t, os.MkdirAll(filepath.Join(dataDir, "backends"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "backends", "backends.scriva"), []byte("v1-store"), 0o600))
	l := &schema.Ledger{
		SchemaVersion: 1,
		BinaryVersion: "1.0.0",
		History:       []schema.HistoryEntry{{From: 0, To: 1, Migration: "baseline-fresh"}},
	}
	require.NoError(t, schema.Save(dataDir, l))

	svc := &fakeSvc{}
	clk := newFakeClock()
	out := &bytes.Buffer{}

	probe := &fakeProbe{fn: func() (Health, error) {
		if svc.count() == 1 {
			// New binary started, but health check fails
			return Health{}, errors.New("connection refused (crash on boot)")
		}
		// Before update or after rollback
		return Health{Status: "ok", Version: "1.0.0"}, nil
	}}

	opts := Options{
		CurrentVersion: "1.0.0",
		TargetVersion:  "2.0.0",
		InstallBin:     binPath,
		DataDir:        dataDir,
		Stdout:         out,
		ReadyTimeout:   5 * time.Second,
		Migrate: func() error {
			// Migration changes data to v2
			_ = os.WriteFile(filepath.Join(dataDir, "backends", "backends.scriva"), []byte("v2-store"), 0o600)
			return nil
		},
	}

	tx := &txn{
		opts:   opts,
		svc:    svc,
		probe:  probe,
		inst:   fsInstaller{bin: binPath},
		clock:  clk,
		target: "2.0.0",
	}

	tx.capture(context.Background())
	rb, err := tx.apply(context.Background(), stagedPath)

	require.Error(t, err)
	require.True(t, rb)

	var rt *ReadinessTimeoutError
	require.ErrorAs(t, err, &rt)

	// Binary restored to old
	binContent, err := os.ReadFile(binPath)
	require.NoError(t, err)
	require.Equal(t, "old-bin", string(binContent))

	// Data restored to v1
	storeContent, err := os.ReadFile(filepath.Join(dataDir, "backends", "backends.scriva"))
	require.NoError(t, err)
	require.Equal(t, "v1-store", string(storeContent))

	// Service was stopped (1), started on v2 (1), restarted on rollback (2)
	require.Equal(t, 1, svc.stops)
	require.Equal(t, 2, svc.count())
}

func TestTxnFaultRestoreFailureMessaging(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	binPath := filepath.Join(dir, "warden")
	stagedPath := filepath.Join(dir, "new")

	require.NoError(t, os.WriteFile(binPath, []byte("old-bin"), 0o755))
	require.NoError(t, os.WriteFile(stagedPath, []byte("new-bin"), 0o755))

	svc := &fakeSvc{}
	clk := newFakeClock()
	out := &bytes.Buffer{}

	probe := &fakeProbe{fn: func() (Health, error) {
		if svc.count() == 1 {
			return Health{}, errors.New("daemon crashed")
		}
		return Health{Status: "ok", Version: "1.0.0"}, nil
	}}

	// Custom installer that fails on Restore
	failingInst := &faultyInstaller{
		bin:        binPath,
		restoreErr: errors.New("permission denied: file is locked by kernel"),
	}

	opts := Options{
		CurrentVersion: "1.0.0",
		TargetVersion:  "2.0.0",
		InstallBin:     binPath,
		DataDir:        dataDir,
		Stdout:         out,
		ReadyTimeout:   3 * time.Second,
	}

	tx := &txn{
		opts:   opts,
		svc:    svc,
		probe:  probe,
		inst:   failingInst,
		clock:  clk,
		target: "2.0.0",
	}

	tx.capture(context.Background())
	rb, err := tx.apply(context.Background(), stagedPath)

	require.Error(t, err)
	require.False(t, rb, "rollback did not succeed")

	var fe *FailureError
	require.ErrorAs(t, err, &fe)
	require.Error(t, fe.Rollback)

	var rbe *RollbackError
	require.ErrorAs(t, err, &rbe)

	errMsg := err.Error()
	require.Contains(t, errMsg, "ROLLBACK FAILED")
	require.Contains(t, errMsg, "manual recovery:")
	require.Contains(t, errMsg, "restore binary from")
	require.Contains(t, errMsg, "restart the warden service")
}

type faultyInstaller struct {
	bin        string
	restoreErr error
}

func (f *faultyInstaller) Swap(newBin string) (string, error) {
	return swapBinary(f.bin, newBin)
}

func (f *faultyInstaller) Restore(backup string) error {
	if f.restoreErr != nil {
		return f.restoreErr
	}
	return restoreBackup(f.bin, backup)
}

func (f *faultyInstaller) Discard(backup string) {
	if backup != "" {
		_ = os.Remove(backup)
	}
}
