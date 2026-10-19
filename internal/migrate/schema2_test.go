package migrate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/srjn45/scriva/engine"
	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/ownerlock"
	"github.com/srjn45/warden/internal/schema"
	"github.com/srjn45/warden/internal/wardendb"
)

var t0 = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

func writeFile(t *testing.T, dir, rel, body string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o700))
	require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
}

// legacyDir builds a minimal schema-1 data dir.
func legacyDir(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	require.NoError(t, schema.Save(d, &schema.Ledger{SchemaVersion: 1, BinaryVersion: "v9", History: []schema.HistoryEntry{}}))
	validStore(t, d, "agents-db", "agents", map[string]any{"v": 1})
	validStore(t, d, "pipelines-db", "pipelines", map[string]any{"v": 1})
	writeFile(t, d, "snapshots/x.json", `{}`)
	writeFile(t, d, "snapshots/x.transcript", "transcript bytes")
	writeFile(t, d, "audit.jsonl", "audit\n")
	writeFile(t, d, "prompts/p.txt", "p")
	return d
}

// validStore creates a clean, closed ScrivaDB collection at <d>/<store>/<col>.
func validStore(t *testing.T, d, store, col string, rec map[string]any) {
	t.Helper()
	storeDir := filepath.Join(d, filepath.FromSlash(store))
	// Standalone legacy collections require an existing database directory.
	require.NoError(t, os.MkdirAll(storeDir, 0o700))
	c, err := engine.OpenCollection(col, storeDir, engine.CollectionConfig{})
	require.NoError(t, err)
	_, _, err = c.Insert(rec)
	require.NoError(t, err)
	require.NoError(t, c.Close())
}

func TestWardenDBDirNamePinned(t *testing.T) {
	require.Equal(t, wardendb.DirName, schema.WardenDBDir)
	require.Equal(t, wardendb.SchemaVersion, schema.Schema2)
}

func TestPlanSchema2_ReadOnlyAndClassifies(t *testing.T) {
	d := legacyDir(t)
	writeFile(t, d, "mystery/blob", "x")
	writeFile(t, d, "config.yaml", "a: b")
	before := snapshotTree(t, d)

	p, err := PlanSchema2(d, t0)
	require.NoError(t, err)
	require.False(t, p.Blocked, "%+v", p.Findings)
	require.Equal(t, filepath.Join(d, "backups", "pre-schema2-20261011T120000Z"), p.BackupDir)
	require.False(t, p.Target.Exists)
	require.Contains(t, p.Target.Collections, "agents")

	byPath := map[string]InventoryEntry{}
	for _, e := range p.Sources {
		byPath[e.Path] = e
	}
	require.Equal(t, DispositionMigrated, byPath["agents-db"].Disposition)
	require.Equal(t, DispositionExternal, byPath["audit.jsonl"].Disposition)
	require.Equal(t, DispositionExternal, byPath["config.yaml"].Disposition)
	require.Equal(t, DispositionUnclassified, byPath["mystery"].Disposition)
	// transcripts are external and not counted as migrated bytes
	require.Equal(t, 1, byPath["snapshots"].Files)

	var notes int
	for _, f := range p.Findings {
		if f.Severity == SeverityRepairable && f.Store == "mystery" {
			notes++
		}
	}
	require.Equal(t, 1, notes)
	require.Equal(t, before, snapshotTree(t, d), "plan must not write")
}

func TestPlanSchema2_Blocks(t *testing.T) {
	cases := map[string]func(t *testing.T, d string){
		"warden-db exists": func(t *testing.T, d string) {
			require.NoError(t, os.MkdirAll(filepath.Join(d, "warden-db"), 0o700))
		},
		"already schema 2": func(t *testing.T, d string) {
			require.NoError(t, schema.Save(d, &schema.Ledger{SchemaVersion: 2}))
		},
		"interrupted": func(t *testing.T, d string) {
			require.NoError(t, schema.Save(d, &schema.Ledger{SchemaVersion: 1, InProgress: &schema.InProgress{Migration: "m"}}))
		},
		"no ledger": func(t *testing.T, d string) { require.NoError(t, os.Remove(schema.Path(d))) },
		"corrupt ledger": func(t *testing.T, d string) {
			require.NoError(t, os.WriteFile(schema.Path(d), []byte("{nope"), 0o600))
		},
		"symlink in source": func(t *testing.T, d string) {
			require.NoError(t, os.Symlink("/etc/hostname", filepath.Join(d, "agents-db", "link")))
		},
		"backup dir exists": func(t *testing.T, d string) {
			require.NoError(t, os.MkdirAll(filepath.Join(d, "backups", "pre-schema2-20261011T120000Z"), 0o700))
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			d := legacyDir(t)
			mutate(t, d)
			p, err := PlanSchema2(d, t0)
			require.NoError(t, err)
			require.True(t, p.Blocked, "findings: %+v", p.Findings)
		})
	}
}

func TestBeginSchema2_BackupAndManifest(t *testing.T) {
	d := legacyDir(t)
	before := snapshotTree(t, d)

	run, err := BeginSchema2(d, "v10", t0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = run.Release() })

	m := run.Manifest
	require.Equal(t, 1, m.From)
	require.Equal(t, 2, m.To)
	require.NotEmpty(t, m.ID)
	paths := map[string]bool{}
	for _, f := range m.Backup.Files {
		paths[f.Path] = true
		require.Len(t, f.SHA256, 64)
	}
	var sawSeg bool
	for p := range paths {
		sawSeg = sawSeg || strings.HasPrefix(p, "agents-db/agents/seg_")
	}
	require.True(t, sawSeg)
	require.True(t, paths["schema.json"])
	require.False(t, paths["snapshots/x.transcript"], "external transcripts are not copied")
	require.False(t, paths["audit.jsonl"])
	require.NotEmpty(t, m.Backup.LedgerSHA256)

	got, err := VerifySchema2Backup(m.Backup.Dir)
	require.NoError(t, err)
	require.Equal(t, m.ID, got.ID)

	// manifest and copies are read-only
	fi, err := os.Stat(filepath.Join(m.Backup.Dir, Schema2ManifestName))
	require.NoError(t, err)
	require.Zero(t, fi.Mode().Perm()&0o222)

	// legacy tree untouched except for the new backups/ dir and the lock file
	require.NoError(t, run.Release())
	after := snapshotTree(t, d)
	for k, v := range before {
		require.Equal(t, v, after[k], k)
	}
	_, hasDB := after["warden-db"]
	require.False(t, hasDB)
}

func TestBeginSchema2_RefusesWhileOwned(t *testing.T) {
	d := legacyDir(t)
	lock, err := ownerlock.Acquire(d, ownerlock.Info{Kind: ownerlock.KindDaemon, Version: "v9"})
	require.NoError(t, err)
	defer lock.Release()
	// same-process lock probes as self; a second Acquire must fail.
	_, err = BeginSchema2(d, "v10", t0)
	require.Error(t, err)
	_, statErr := os.Stat(filepath.Join(d, "backups"))
	require.True(t, os.IsNotExist(statErr), "no backup may be created while owned")
}

func TestBeginSchema2_BlockedLeavesNothing(t *testing.T) {
	d := legacyDir(t)
	require.NoError(t, os.MkdirAll(filepath.Join(d, "warden-db"), 0o700))
	_, err := BeginSchema2(d, "v10", t0)
	require.ErrorIs(t, err, ErrSchema2Blocked)
	_, statErr := os.Stat(filepath.Join(d, "backups"))
	require.True(t, os.IsNotExist(statErr))
}

func TestBeginSchema2_NeverReusesBackup(t *testing.T) {
	d := legacyDir(t)
	run, err := BeginSchema2(d, "v10", t0)
	require.NoError(t, err)
	require.NoError(t, run.Release())
	_, err = BeginSchema2(d, "v10", t0) // same second => same dir
	require.ErrorIs(t, err, ErrSchema2Blocked)
}

func TestVerifySchema2Backup_Detects(t *testing.T) {
	setup := func(t *testing.T) string {
		d := legacyDir(t)
		run, err := BeginSchema2(d, "v10", t0)
		require.NoError(t, err)
		require.NoError(t, run.Release())
		return run.Manifest.Backup.Dir
	}
	t.Run("incomplete", func(t *testing.T) {
		dir := setup(t)
		require.NoError(t, os.Chmod(dir, 0o700))
		require.NoError(t, os.Remove(filepath.Join(dir, Schema2ManifestName)))
		_, err := VerifySchema2Backup(dir)
		require.ErrorIs(t, err, ErrBackupInvalid)
	})
	t.Run("tampered", func(t *testing.T) {
		dir := setup(t)
		segs, err := filepath.Glob(filepath.Join(dir, "data", "agents-db", "agents", "seg_*.ndjson"))
		require.NoError(t, err)
		require.NotEmpty(t, segs)
		p := segs[0]
		require.NoError(t, os.Chmod(p, 0o600))
		require.NoError(t, os.WriteFile(p, []byte("changed"), 0o600))
		_, err = VerifySchema2Backup(dir)
		require.ErrorIs(t, err, ErrBackupInvalid)
	})
	t.Run("extra file", func(t *testing.T) {
		dir := setup(t)
		writeFile(t, dir, "data/extra", "x")
		_, err := VerifySchema2Backup(dir)
		require.ErrorIs(t, err, ErrBackupInvalid)
	})
	t.Run("missing file", func(t *testing.T) {
		dir := setup(t)
		require.NoError(t, os.Remove(filepath.Join(dir, "data", "schema.json")))
		_, err := VerifySchema2Backup(dir)
		require.ErrorIs(t, err, ErrBackupInvalid)
	})
}

// snapshotTree maps relative path -> content for every regular file, skipping
// the backups tree and the transient owner lock.
func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	require.NoError(t, filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if rel == ownerlock.FileName || len(rel) >= 7 && rel[:7] == "backups" {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[rel] = string(b)
		return nil
	}))
	return out
}
