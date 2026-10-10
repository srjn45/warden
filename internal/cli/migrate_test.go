package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/migrate"
	"github.com/srjn45/warden/internal/ownerlock"
	"github.com/srjn45/warden/internal/schema"
)

func TestMigrateCommand_CheckClean(t *testing.T) {
	dataDir := t.TempDir()

	// Initial clean ledger
	l := &schema.Ledger{
		SchemaVersion: schema.SchemaVersion,
		BinaryVersion: "9.29.0",
		History:       []schema.HistoryEntry{},
	}
	require.NoError(t, schema.Save(dataDir, l))

	cmd := newMigrateCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"--check", "--data-dir", dataDir})

	require.NoError(t, cmd.ExecuteContext(context.Background()))
	require.Contains(t, buf.String(), "all stores clean")
}

func TestMigrateCommand_CheckJSON(t *testing.T) {
	dataDir := t.TempDir()

	l := &schema.Ledger{
		SchemaVersion: schema.SchemaVersion,
		BinaryVersion: "9.29.0",
		History:       []schema.HistoryEntry{},
	}
	require.NoError(t, schema.Save(dataDir, l))

	cmd := newMigrateCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"--check", "--json", "--data-dir", dataDir})

	require.NoError(t, cmd.ExecuteContext(context.Background()))

	var rep migrateCheckReport
	require.NoError(t, json.Unmarshal(buf.Bytes(), &rep))
	require.True(t, rep.Clean)
	require.Equal(t, schema.SchemaVersion, rep.CurrentVersion)
	require.Equal(t, schema.SchemaVersion, rep.TargetVersion)
	require.Empty(t, rep.Findings)
}

func TestMigrateCommand_CheckReportsFindingsAndFails(t *testing.T) {
	dataDir := t.TempDir()

	// Create a corrupted store in context
	ctxDir := filepath.Join(dataDir, "context")
	require.NoError(t, os.MkdirAll(filepath.Join(ctxDir, "context"), 0o700))
	badSeg := filepath.Join(ctxDir, "context", "seg_000001.ndjson")
	require.NoError(t, os.WriteFile(badSeg, []byte("{not valid json\n"), 0o600))

	cmd := newMigrateCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"--check", "--data-dir", dataDir})

	err := cmd.ExecuteContext(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "issue(s) requiring repair")
	require.Contains(t, buf.String(), "[BLOCKING] context/context")
	require.Contains(t, buf.String(), "warden repair all --resolve-history=live-wins")
}

func TestMigrateCommand_CheckJSONWithFindings(t *testing.T) {
	dataDir := t.TempDir()

	// Create a corrupted store in context
	ctxDir := filepath.Join(dataDir, "context")
	require.NoError(t, os.MkdirAll(filepath.Join(ctxDir, "context"), 0o700))
	badSeg := filepath.Join(ctxDir, "context", "seg_000001.ndjson")
	require.NoError(t, os.WriteFile(badSeg, []byte("{not valid json\n"), 0o600))

	cmd := newMigrateCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"--check", "--json", "--data-dir", dataDir})

	err := cmd.ExecuteContext(context.Background())
	require.Error(t, err)

	var rep migrateCheckReport
	require.NoError(t, json.Unmarshal(buf.Bytes(), &rep))
	hasBlocking := false
	for _, f := range rep.Findings {
		if f.Severity == migrate.SeverityBlocking {
			hasBlocking = true
		}
	}
	require.True(t, hasBlocking)
}

func TestMigrateCommand_ApplyAndResume(t *testing.T) {
	dataDir := t.TempDir()

	// Apply migration to current SchemaVersion
	cmd := newMigrateCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"--apply", "--data-dir", dataDir})

	require.NoError(t, cmd.ExecuteContext(context.Background()))
	require.Contains(t, buf.String(), "migrated")

	updated, err := schema.Load(dataDir)
	require.NoError(t, err)
	require.Equal(t, schema.SchemaVersion, updated.SchemaVersion)
}

func TestMigrateCommand_ApplyRefusedWhenDaemonRunning(t *testing.T) {
	dataDir := t.TempDir()

	// Acquire owner lock simulating a running daemon
	lock, err := ownerlock.Acquire(dataDir, ownerlock.Info{
		Kind:    ownerlock.KindDaemon,
		Version: "9.28.0",
	})
	require.NoError(t, err)
	defer lock.Release()

	cmd := newMigrateCmd()
	cmd.SetArgs([]string{"--apply", "--data-dir", dataDir})

	err = cmd.ExecuteContext(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "cannot apply migrations while daemon is running")
}

func TestMigrateCommand_Restore(t *testing.T) {
	dataDir := t.TempDir()

	// Snapshot
	snapDir := filepath.Join(dataDir, "backups", "snap-test")
	require.NoError(t, os.MkdirAll(snapDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(snapDir, "sentinel.txt"), []byte("restored_content"), 0o600))

	// In-progress ledger
	l := &schema.Ledger{
		SchemaVersion: 1,
		BinaryVersion: "9.28.0",
		InProgress: &schema.InProgress{
			Migration: "0002",
			Step:      "step1",
			Snapshot:  "backups/snap-test",
		},
	}
	require.NoError(t, schema.Save(dataDir, l))

	cmd := newMigrateCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"--restore", "--data-dir", dataDir})

	require.NoError(t, cmd.ExecuteContext(context.Background()))
	require.Contains(t, buf.String(), "restored pre-migration state")

	content, err := os.ReadFile(filepath.Join(dataDir, "sentinel.txt"))
	require.NoError(t, err)
	require.Equal(t, "restored_content", string(content))

	lAfter, err := schema.Load(dataDir)
	require.NoError(t, err)
	require.Nil(t, lAfter.InProgress)
}

func TestMigrateCommand_MutuallyExclusiveFlags(t *testing.T) {
	cmd := newMigrateCmd()
	cmd.SetArgs([]string{"--check", "--apply"})
	require.Error(t, cmd.ExecuteContext(context.Background()))
}
