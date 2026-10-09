package backendstore

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// treeBytes returns every file under dir (except LOCK) keyed by relative path.
func treeBytes(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	require.NoError(t, filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() == "LOCK" {
			return err
		}
		b, rerr := os.ReadFile(p)
		rel, _ := filepath.Rel(dir, p)
		out[rel] = string(b)
		return rerr
	}))
	return out
}

func quiet() Options { return Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))} }

func TestOpenCleanIsNoOp(t *testing.T) {
	dir := t.TempDir()
	want := buildRegistry(t, dir)
	opts := quiet()
	opts.BackupDir = t.TempDir()
	s, res, err := Open(dir, opts)
	require.NoError(t, err)
	defer s.Close()
	require.Nil(t, res)
	requireRegistryPreserved(t, want, snapshotRegistry(t, s))
	ents, _ := os.ReadDir(opts.BackupDir)
	require.Empty(t, ents)
}

func TestOpenRecoversRegressionAndAudits(t *testing.T) {
	dir, want := damagedRegistry(t, "backends", "role_tiers")
	opts := quiet()
	opts.BackupDir = t.TempDir()
	rep, err := Verify(context.Background(), dir)
	require.NoError(t, err)
	require.True(t, rep.Recoverable())
	require.False(t, rep.Clean())

	s, res, err := Open(dir, opts)
	require.NoError(t, err)
	defer s.Close()
	require.True(t, res.Recovered)
	require.NotEmpty(t, res.Discarded)
	requireRegistryPreserved(t, want, snapshotRegistry(t, s))

	// Audit artefacts: verified backup + JSON report naming every discard.
	require.DirExists(t, res.BackupPath)
	raw, err := os.ReadFile(res.ReportPath)
	require.NoError(t, err)
	var doc struct {
		Rule      string
		Discarded []DiscardedRevision
	}
	require.NoError(t, json.Unmarshal(raw, &doc))
	require.Equal(t, ResolutionRule, doc.Rule)
	require.Len(t, doc.Discarded, len(res.Discarded))

	// Idempotent: a second run is a no-op.
	require.NoError(t, s.Close())
	res2, err := Repair(context.Background(), dir, opts)
	require.NoError(t, err)
	require.False(t, res2.Recovered)
	require.Empty(t, res2.BackupPath)
}

func TestBackupRestoresIndependently(t *testing.T) {
	dir, want := damagedRegistry(t, "backends")
	orig := treeBytes(t, dir)
	opts := quiet()
	opts.BackupDir = t.TempDir()
	res, err := Repair(context.Background(), dir, opts)
	require.NoError(t, err)
	require.Equal(t, orig, treeBytes(t, res.BackupPath), "backup is a byte-identical copy of the pre-repair store")

	// Copy the backup to a fresh dir: it is the original damaged store, and
	// recovering it independently yields the same preserved registry.
	fresh := t.TempDir()
	require.NoError(t, copyDirVerified(context.Background(), res.BackupPath, fresh))
	_, err = NewStore(fresh)
	require.Error(t, err)
	_, _, err = Open(fresh, Options{BackupDir: t.TempDir(), Logger: quiet().Logger})
	require.NoError(t, err)
	_ = want
}

// ambiguous: the "stale" line carries a NEWER timestamp than the newest
// revision -- a second writer's later, divergent write. Unresolvable.
func TestAmbiguousRegressionTypedErrorNothingMutated(t *testing.T) {
	dir, _ := damagedRegistry(t, "backends")
	seg := filepath.Join(dir, "backends", "seg_000001.ndjson")
	b, err := os.ReadFile(seg)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	// Make the final replayed lines newer than everything else.
	for i := len(lines) - 2; i < len(lines); i++ {
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(lines[i]), &m))
		m["ts"] = "2999-01-01T00:00:00Z"
		nb, _ := json.Marshal(m)
		lines[i] = string(nb)
	}
	require.NoError(t, os.WriteFile(seg, []byte(strings.Join(lines, "\n")+"\n"), 0o600))
	before := treeBytes(t, dir)

	opts := quiet()
	opts.BackupDir = t.TempDir()
	_, _, err = Open(dir, opts)
	require.Error(t, err)
	var rre *RecoveryRequiredError
	require.True(t, errors.As(err, &rre))
	require.ErrorIs(t, err, ErrRecoveryRequired)
	require.Equal(t, RepairCommand, rre.RepairCommand)
	require.Equal(t, "backends", rre.Collections[0].Name)
	require.Contains(t, rre.Severities, "conflict")
	require.NotEmpty(t, rre.ReportPath)
	require.Empty(t, rre.BackupPath)
	require.Contains(t, err.Error(), RepairCommand)
	require.Equal(t, before, treeBytes(t, dir), "ambiguous history must not mutate the store")
}

func TestRepairRollsBackOnFailure(t *testing.T) {
	dir, _ := damagedRegistry(t, "backends")
	before := treeBytes(t, dir)
	opts := quiet()
	opts.BackupDir = t.TempDir()
	// Make the engine rebuild fail: a directory squats on the engine backup path.
	fixedNow := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	opts.Now = func() time.Time { return fixedNow }
	stamp := fixedNow.Format("20060102T150405Z")
	eng := filepath.Join(opts.BackupDir, "engine-"+stamp)
	require.NoError(t, os.MkdirAll(eng, 0o700))
	require.NoError(t, os.Chmod(eng, 0o500))
	t.Cleanup(func() { _ = os.Chmod(eng, 0o700) })
	if os.Geteuid() == 0 {
		t.Skip("permission-based failure injection needs non-root")
	}

	_, err := Repair(context.Background(), dir, opts)
	require.Error(t, err)
	var rre *RecoveryRequiredError
	require.True(t, errors.As(err, &rre))
	require.NotEmpty(t, rre.BackupPath)
	require.Equal(t, before, treeBytes(t, dir), "failed repair restores the store byte-identically")
}

func TestRepairRefusesOwnedStore(t *testing.T) {
	dir := t.TempDir()
	buildRegistry(t, dir)
	s, err := NewStore(dir)
	require.NoError(t, err)
	defer s.Close()
	_, err = Repair(context.Background(), dir, quiet())
	require.ErrorIs(t, err, ErrOwned)
}
