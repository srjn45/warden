package store

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDiagnoseAndRebuildSessionsSkipsCorruption(t *testing.T) {
	ctx := context.Background()
	src := t.TempDir()
	fs, err := NewFileStore(src)
	require.NoError(t, err)
	active := sample()
	active.ID = "active-1"
	require.NoError(t, fs.Insert(ctx, active))
	closed := sample()
	closed.ID = "closed-1"
	closed.Name = ""
	require.NoError(t, fs.Insert(ctx, closed))
	require.NoError(t, fs.Archive(ctx, closed.ID))
	require.NoError(t, fs.Close(ctx))

	segments, err := filepath.Glob(filepath.Join(src, "sessions-db", "active", "seg_*.ndjson"))
	require.NoError(t, err)
	require.NotEmpty(t, segments)
	f, err := os.OpenFile(segments[len(segments)-1], os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = f.WriteString("{broken-json}\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())

	report, err := DiagnoseSessions(ctx, src)
	require.NoError(t, err)
	require.Len(t, report.Active, 1)
	require.Len(t, report.Closed, 1)
	require.Len(t, report.Skipped, 1)

	dst := t.TempDir()
	require.NoError(t, RebuildSessions(ctx, dst, report))
	rebuilt, err := DiagnoseSessions(ctx, dst)
	require.NoError(t, err)
	require.Empty(t, rebuilt.Skipped)
	require.Equal(t, []string{"active-1"}, []string{rebuilt.Active[0].ID})
	require.Equal(t, []string{"closed-1"}, []string{rebuilt.Closed[0].ID})
}

func TestNewFileStoreRestoresInterruptedRepairGap(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	require.NoError(t, err)
	s := sample()
	s.ID = "survives-crash"
	require.NoError(t, fs.Insert(ctx, s))
	require.NoError(t, fs.Close(ctx))

	original := filepath.Join(dir, "sessions-db.pre-repair-test")
	require.NoError(t, os.Rename(filepath.Join(dir, "sessions-db"), original))
	b, err := json.Marshal(repairState{Original: original, HadOriginal: true})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, repairStateName), b, 0o600))

	fs, err = NewFileStore(dir)
	require.NoError(t, err)
	defer fs.Close(ctx)
	got, err := fs.Get(ctx, s.ID)
	require.NoError(t, err)
	require.Equal(t, s.ID, got.ID)
	_, err = os.Stat(filepath.Join(dir, repairStateName))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestDiagnoseSessionsDoesNotMutateSource(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	require.NoError(t, err)
	require.NoError(t, fs.Insert(ctx, sample()))
	require.NoError(t, fs.Close(ctx))
	segment, err := filepath.Glob(filepath.Join(dir, "sessions-db", "active", "seg_*.ndjson"))
	require.NoError(t, err)
	before, err := os.ReadFile(segment[0])
	require.NoError(t, err)
	_, err = DiagnoseSessions(ctx, dir)
	require.NoError(t, err)
	after, err := os.ReadFile(segment[0])
	require.NoError(t, err)
	require.Equal(t, before, after)
}

func TestDiagnoseSessionsReconcilesInterruptedArchive(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	require.NoError(t, err)
	s := sample()
	s.ID = "both-1"
	require.NoError(t, fs.Insert(ctx, s))
	require.NoError(t, fs.Close(ctx))
	active, err := filepath.Glob(filepath.Join(dir, "sessions-db", "active", "seg_*.ndjson"))
	require.NoError(t, err)
	closed, err := filepath.Glob(filepath.Join(dir, "sessions-db", "closed", "seg_*.ndjson"))
	require.NoError(t, err)
	b, err := os.ReadFile(active[0])
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(closed[0], b, 0o600))
	report, err := DiagnoseSessions(ctx, dir)
	require.NoError(t, err)
	require.Empty(t, report.Active)
	require.Len(t, report.Closed, 1)
	require.Equal(t, []string{"both-1"}, report.Reconciled)
}
