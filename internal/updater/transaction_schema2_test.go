package updater

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/schema"
)

// schema2Fixture is a schema-1 data dir plus a fake "cutover" that behaves like
// the new binary's `migrate --apply` from the updater's point of view: it
// promotes warden-db, takes the immutable pre-schema-2 backup, and advances the
// ledger. The real cutover is exercised in internal/migrate and internal/cli;
// these tests pin what the UPDATER does around it.
type schema2Fixture struct {
	dataDir, binPath, stagedPath string
	svc                          *fakeSvc
	out                          *bytes.Buffer
}

func newSchema2Fixture(t *testing.T) *schema2Fixture {
	t.Helper()
	dir := t.TempDir()
	f := &schema2Fixture{dataDir: filepath.Join(dir, "data"), binPath: filepath.Join(dir, "warden"), stagedPath: filepath.Join(dir, "new"), svc: &fakeSvc{}, out: &bytes.Buffer{}}
	require.NoError(t, os.WriteFile(f.binPath, []byte("old-bin"), 0o755))
	require.NoError(t, os.WriteFile(f.stagedPath, []byte("new-bin"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(f.dataDir, "agents-db", "agents"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(f.dataDir, "agents-db", "agents", "seg_000001.ndjson"), []byte("legacy-agents\n"), 0o600))
	require.NoError(t, schema.Save(f.dataDir, &schema.Ledger{SchemaVersion: 1, BinaryVersion: "10.0.1", History: []schema.HistoryEntry{}}))
	return f
}

// cutover simulates a successful `new migrate --apply`.
func (f *schema2Fixture) cutover(t *testing.T) func() error {
	return func() error {
		require.NoError(t, os.MkdirAll(filepath.Join(f.dataDir, schema.WardenDBDir, "agents"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(f.dataDir, schema.WardenDBDir, "agents", "written-by-v11"), []byte("v11-data\n"), 0o600))
		bk := filepath.Join(f.dataDir, "backups", schema2BackupPrefix+"20261018T100000Z")
		require.NoError(t, os.MkdirAll(filepath.Join(bk, "data"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(bk, "MANIFEST.json"), []byte("{}"), 0o400))
		l, err := schema.Load(f.dataDir)
		require.NoError(t, err)
		l.InProgress, l.SchemaVersion = nil, 2
		l.History = append(l.History, schema.HistoryEntry{From: 1, To: 2, Migration: "0002-schema2-cutover", Backup: bk})
		return schema.Save(f.dataDir, l)
	}
}

func (f *schema2Fixture) txn(health func(restarts int) Health, migrate func() error) *txn {
	return &txn{
		opts: Options{
			CurrentVersion: "10.0.1", TargetVersion: "11.0.0", InstallBin: f.binPath, DataDir: f.dataDir, Stdout: f.out,
			ReadyTimeout: 5 * time.Second, Migrate: migrate,
			TargetSchema:  func(context.Context, string) (int, error) { return 2, nil },
			CurrentSchema: 1,
		},
		svc: f.svc, probe: &fakeProbe{fn: func() (Health, error) { return health(f.svc.count()), nil }},
		inst: fsInstaller{bin: f.binPath}, clock: newFakeClock(), target: "11.0.0",
	}
}

// Happy path: stop → snapshot → swap → cutover → restart → healthy ON SCHEMA 2.
func TestTxnSchema2UpdateRunsCutoverAndRequiresSchema2Health(t *testing.T) {
	f := newSchema2Fixture(t)
	tx := f.txn(func(restarts int) Health {
		if restarts == 0 {
			return Health{Status: "ok", Version: "10.0.1", SchemaVersion: 1}
		}
		return Health{Status: "ok", Version: "11.0.0", SchemaVersion: 2}
	}, f.cutover(t))
	tx.capture(context.Background())
	rb, err := tx.apply(context.Background(), f.stagedPath)
	require.NoError(t, err)
	require.False(t, rb)
	require.Equal(t, 1, f.svc.stops, "daemon is stopped before the cutover")

	l, err := schema.Load(f.dataDir)
	require.NoError(t, err)
	require.Equal(t, 2, l.SchemaVersion)
	require.Nil(t, l.InProgress, "no snapshot marker may reach the new daemon's boot guard")
	require.Equal(t, schema.VerdictStart, schema.Decide(l.SchemaVersion, schema.Schema2, 1, false))
	require.FileExists(t, filepath.Join(f.dataDir, "agents-db", "agents", "seg_000001.ndjson"), "legacy stores stay intact")
}

// A new daemon that comes up healthy but on the WRONG schema (or never reports
// one) is not a finished update; the rollback must leave a bootable schema-1
// dir: ledger restored, legacy stores intact, warden-db PRESERVED (not deleted)
// outside the data root so no binary sees two authorities.
func TestTxnSchema2RollbackAfterCutoverPreservesWardenDB(t *testing.T) {
	f := newSchema2Fixture(t)
	tx := f.txn(func(restarts int) Health {
		if restarts == 1 { // the new daemon: right version, wrong schema
			return Health{Status: "ok", Version: "11.0.0", SchemaVersion: 1}
		}
		return Health{Status: "ok", Version: "10.0.1", SchemaVersion: 1}
	}, f.cutover(t))
	tx.capture(context.Background())
	rb, err := tx.apply(context.Background(), f.stagedPath)
	require.Error(t, err)
	require.True(t, rb, "rollback must succeed")
	var ws *WrongSchemaError
	require.ErrorAs(t, err, &ws)

	b, _ := os.ReadFile(f.binPath)
	require.Equal(t, "old-bin", string(b))
	l, err := schema.Load(f.dataDir)
	require.NoError(t, err)
	require.Equal(t, 1, l.SchemaVersion)
	require.Nil(t, l.InProgress)
	require.False(t, schema.WardenDBPresent(f.dataDir), "the previous binary must not find two authorities")
	require.Equal(t, schema.VerdictStart, schema.Decide(l.SchemaVersion, 1, 1, l.MigrationInterrupted()),
		"rolled-back data must pass the previous (schema-1) binary's boot guard")
	require.FileExists(t, filepath.Join(f.dataDir, "agents-db", "agents", "seg_000001.ndjson"))

	moved, err := filepath.Glob(filepath.Join(f.dataDir, "backups", "abandoned-schema2-*", "agents", "written-by-v11"))
	require.NoError(t, err)
	require.Len(t, moved, 1, "whatever the failed schema-2 daemon wrote stays recoverable")
	require.DirExists(t, filepath.Join(f.dataDir, "backups", schema2BackupPrefix+"20261018T100000Z"), "the immutable pre-schema-2 backup is never touched")
}

// A failing cutover (e.g. strict preflight refusal) leaves the old binary and
// the untouched legacy data in place and never starts the new daemon.
func TestTxnSchema2FailedCutoverRollsBackWithoutStartingNewDaemon(t *testing.T) {
	f := newSchema2Fixture(t)
	tx := f.txn(func(restarts int) Health {
		return Health{Status: "ok", Version: "10.0.1", SchemaVersion: 1}
	}, func() error {
		return os.ErrInvalid // preflight refusal: nothing was promoted
	})
	rb, err := func() (bool, error) {
		tx.capture(context.Background())
		return tx.apply(context.Background(), f.stagedPath)
	}()
	require.Error(t, err)
	require.True(t, rb)
	b, _ := os.ReadFile(f.binPath)
	require.Equal(t, "old-bin", string(b))
	require.False(t, schema.WardenDBPresent(f.dataDir))
	l, err := schema.Load(f.dataDir)
	require.NoError(t, err)
	require.Equal(t, 1, l.SchemaVersion)
	require.Nil(t, l.InProgress)
}

// The immutable pre-schema-2 backup shares the "pre-" prefix with updater
// snapshots; pruning or `wd rollback` must never treat it as one.
func TestPruneAndLatestSnapshotIgnoreSchema2Backup(t *testing.T) {
	d := t.TempDir()
	mk := func(name string) string {
		p := filepath.Join(d, "backups", name)
		require.NoError(t, os.MkdirAll(p, 0o700))
		return p
	}
	s2 := mk(schema2BackupPrefix + "20200101T000000Z") // very old: would be pruned by age
	require.NoError(t, os.WriteFile(filepath.Join(s2, "MANIFEST.json"), []byte("{}"), 0o400))
	mk("pre-v1.0.0-20200101T000000Z")
	mk("pre-v1.1.0-20200102T000000Z")
	mk("pre-v1.2.0-20200103T000000Z")
	newest := mk("pre-v1.3.0-20200104T000000Z")
	_ = mk(schema2BackupPrefix + "20300101T000000Z") // newest by name; must not be picked as a rollback snapshot

	got, err := LatestSnapshot(d)
	require.NoError(t, err)
	require.Equal(t, newest, got)

	_, err = PruneSnapshots(d, newFakeClock(), 2, time.Hour)
	require.NoError(t, err)
	require.DirExists(t, s2)
	require.DirExists(t, filepath.Join(d, "backups", schema2BackupPrefix+"20300101T000000Z"))
	ents, _ := os.ReadDir(filepath.Join(d, "backups"))
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	require.Contains(t, strings.Join(names, " "), "pre-v1.3.0")
}
