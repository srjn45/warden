package wardendb

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// openVerified opens dataDir strictly, reads the seeded records back and
// requires the owning handle to verify without blocking findings.
func openVerified(t *testing.T, dataDir string) {
	t.Helper()
	db, err := Open(dataDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	if n := agentCount(t, db); n != 3 {
		t.Fatalf("read %d of 3 records", n)
	}
	res, err := db.Verify(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Health != HealthHealthy {
		t.Fatalf("verify after open = %q (blocking %d, recoverable %d)", res.Health, res.Blocking, res.Recoverable)
	}
}

func TestOnlineBackupRestoreRoundTrip(t *testing.T) {
	src, dst, dest := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "bk")
	seed(t, src)

	rec := &recorder{}
	db := mustOpen(t, src, observed(rec)...)
	if n := agentCount(t, db); n != 3 {
		t.Fatalf("read %d records", n)
	}
	m, err := db.Backup(context.Background(), dest)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if m.Kind != BackupSnapshot || m.SchemaVersion != SchemaVersion || m.Format != BackupFormat || m.Checksum == "" || m.Source != Dir(src) {
		t.Fatalf("manifest = %+v", m)
	}
	if len(m.Collections) != 1 || m.Collections[0].Name != "agents" || len(m.Collections[0].Files) == 0 {
		t.Fatalf("collections = %+v", m.Collections)
	}
	for _, f := range m.allFiles() {
		if len(f.SHA256) != 64 {
			t.Errorf("%s has checksum %q", f.Path, f.SHA256)
		}
	}
	if rec.count(ActionBackup, "") != 1 {
		t.Error("backup was not audited")
	}
	// The owner keeps working after a backup.
	if _, err := db.Handle(); err != nil {
		t.Fatal(err)
	}
	if got, err := VerifyBackup(dest); err != nil || got.Checksum != m.Checksum {
		t.Fatalf("VerifyBackup = %+v, %v", got, err)
	}

	rrec := &recorder{}
	res, err := Restore(context.Background(), dest, dst, RestoreOptions{}, observed(rrec)...)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if res.Dir != Dir(dst) || res.Files != len(m.allFiles()) || res.Replaced != "" || !res.Verify.Health.Openable() {
		t.Fatalf("restore result = %+v (health %q)", res, res.Verify.Health)
	}
	if rrec.count(ActionRestore, "") != 1 {
		t.Error("restore was not audited")
	}
	if st, err := Probe(dst); err != nil || st.Owned {
		t.Fatalf("Restore kept ownership: %+v, %v", st, err)
	}
	openVerified(t, dst)
}

func TestOfflineBackupRestoreRoundTrip(t *testing.T) {
	src, dst, dest := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "bk")
	seed(t, src)
	before := snapshot(t, Dir(src))

	m, err := Backup(context.Background(), src, dest)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	sameSnapshot(t, before, snapshot(t, Dir(src)))
	if m.Kind != BackupCopy || len(m.allFiles()) != len(before) {
		t.Fatalf("manifest lists %d files of %d (%+v)", len(m.allFiles()), len(before), m)
	}
	if st, err := Probe(src); err != nil || st.Owned {
		t.Fatalf("Backup kept ownership: %+v, %v", st, err)
	}

	res, err := Restore(context.Background(), dest, dst, RestoreOptions{})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if res.Verify.Health != HealthHealthy {
		t.Errorf("restored health = %q", res.Verify.Health)
	}
	sameSnapshot(t, before, snapshot(t, Dir(dst)))
	openVerified(t, dst)
}

func TestBackupRefusals(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	seed(t, dataDir)

	// Destination inside warden-db, or not empty.
	if _, err := Backup(ctx, dataDir, filepath.Join(Dir(dataDir), "bk")); !errors.Is(err, ErrBackupDestination) {
		t.Errorf("destination inside warden-db: %v", err)
	}
	full := t.TempDir()
	if err := os.WriteFile(filepath.Join(full, "x"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Backup(ctx, dataDir, full); !errors.Is(err, ErrBackupDestination) {
		t.Errorf("non-empty destination: %v", err)
	}
	if _, err := os.Stat(filepath.Join(full, BackupManifestName)); !errors.Is(err, os.ErrNotExist) {
		t.Error("a refused backup left a manifest behind")
	}
	// No database.
	if _, err := Backup(ctx, t.TempDir(), filepath.Join(t.TempDir(), "bk")); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing database: %v", err)
	}

	// A live database is backed up by its owner, not from the side.
	db := mustOpen(t, dataDir)
	if _, err := Backup(ctx, dataDir, filepath.Join(t.TempDir(), "bk")); !errors.Is(err, ErrLocked) {
		t.Errorf("offline backup of a live database: %v", err)
	}
	if _, err := db.Backup(ctx, filepath.Join(Dir(dataDir), "bk")); !errors.Is(err, ErrBackupDestination) {
		t.Errorf("online backup into warden-db: %v", err)
	}
	if _, err := db.Backup(ctx, full); !errors.Is(err, ErrBackupDestination) {
		t.Errorf("online backup into a non-empty directory: %v", err)
	}
}

func TestVerifyBackupDetectsTampering(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	seed(t, dataDir)
	take := func() (string, *BackupManifest) {
		dest := filepath.Join(t.TempDir(), "bk")
		m, err := Backup(ctx, dataDir, dest)
		if err != nil {
			t.Fatal(err)
		}
		return dest, m
	}
	segOf := func(dest string, m *BackupManifest) string {
		for _, f := range m.allFiles() {
			if filepath.Ext(f.Path) == ".ndjson" {
				return filepath.Join(dest, BackupDataDir, filepath.FromSlash(f.Path))
			}
		}
		t.Fatal("no segment in backup")
		return ""
	}
	rewriteManifest := func(dest string, edit func(*BackupManifest)) {
		p := filepath.Join(dest, BackupManifestName)
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		var m BackupManifest
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		edit(&m)
		b, _ = json.Marshal(&m)
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	cases := map[string]func(dest string, m *BackupManifest){
		"altered file": func(dest string, m *BackupManifest) {
			corruptFirstRecord(t, segOf(dest, m))
		},
		"missing file": func(dest string, m *BackupManifest) {
			if err := os.Remove(segOf(dest, m)); err != nil {
				t.Fatal(err)
			}
		},
		"extra file": func(dest string, _ *BackupManifest) {
			if err := os.WriteFile(filepath.Join(dest, BackupDataDir, "agents", "seg_999999.ndjson"), []byte("{}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"missing manifest": func(dest string, _ *BackupManifest) {
			if err := os.Remove(filepath.Join(dest, BackupManifestName)); err != nil {
				t.Fatal(err)
			}
		},
		"edited manifest": func(dest string, _ *BackupManifest) {
			rewriteManifest(dest, func(m *BackupManifest) { m.Source = "/elsewhere" })
		},
		"other schema version": func(dest string, _ *BackupManifest) {
			rewriteManifest(dest, func(m *BackupManifest) { m.SchemaVersion = SchemaVersion + 1; m.Checksum = m.sum() })
		},
		"escaping path": func(dest string, _ *BackupManifest) {
			rewriteManifest(dest, func(m *BackupManifest) {
				m.Files = append(m.Files, BackupFile{Path: "../outside"})
				m.Checksum = m.sum()
			})
		},
	}
	for name, tamper := range cases {
		dest, m := take()
		tamper(dest, m)
		if _, err := VerifyBackup(dest); !errors.Is(err, ErrBackupInvalid) {
			t.Errorf("%s: VerifyBackup = %v, want ErrBackupInvalid", name, err)
		}
		target := t.TempDir()
		rec := &recorder{}
		if _, err := Restore(ctx, dest, target, RestoreOptions{Overwrite: true, AllowCorrupt: true}, observed(rec)...); !errors.Is(err, ErrBackupInvalid) {
			t.Errorf("%s: Restore = %v, want ErrBackupInvalid", name, err)
		}
		if _, err := os.Stat(Dir(target)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: a refused restore created the target", name)
		}
		if rec.count(ActionRestoreRefused, "") != 1 {
			t.Errorf("%s: refusal was not audited", name)
		}
	}
}

func TestRestoreRefusesToOverwrite(t *testing.T) {
	ctx := context.Background()
	src, dest := t.TempDir(), filepath.Join(t.TempDir(), "bk")
	seed(t, src)
	if _, err := Backup(ctx, src, dest); err != nil {
		t.Fatal(err)
	}

	// A target that already holds data.
	target := t.TempDir()
	seed(t, target)
	before := snapshot(t, Dir(target))
	if _, err := Restore(ctx, dest, target, RestoreOptions{}); !errors.Is(err, ErrRestoreRefused) {
		t.Fatalf("restore over data: %v", err)
	}
	sameSnapshot(t, before, snapshot(t, Dir(target)))
	leftovers, _ := filepath.Glob(filepath.Join(target, "*restore*"))
	if len(leftovers) != 0 {
		t.Errorf("refused restore left staging behind: %v", leftovers)
	}

	// A live target is never overwritten, even when asked.
	db := mustOpen(t, target)
	if _, err := Restore(ctx, dest, target, RestoreOptions{Overwrite: true}); !errors.Is(err, ErrLocked) {
		t.Fatalf("restore over a live database: %v", err)
	}
	if n := agentCount(t, db); n != 3 {
		t.Errorf("owner reads %d records", n)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before = snapshot(t, Dir(target))

	// Asked explicitly: the previous contents are moved aside, not deleted.
	res, err := Restore(ctx, dest, target, RestoreOptions{Overwrite: true})
	if err != nil {
		t.Fatalf("Restore with Overwrite: %v", err)
	}
	if res.Replaced == "" || inside(Dir(target), res.Replaced) {
		t.Fatalf("Replaced = %q", res.Replaced)
	}
	sameSnapshot(t, before, snapshot(t, res.Replaced))
	openVerified(t, target)
}

func TestRestoreRefusesCorruptBackupUnlessAllowed(t *testing.T) {
	ctx := context.Background()
	src, dest := t.TempDir(), filepath.Join(t.TempDir(), "bk")
	seed(t, src)
	corruptFirstRecord(t, newestSegment(t, src, "agents"))
	damaged := snapshot(t, Dir(src))

	// A damaged database can still be preserved exactly.
	if _, err := Backup(ctx, src, dest); err != nil {
		t.Fatalf("Backup of a damaged database: %v", err)
	}
	sameSnapshot(t, damaged, snapshot(t, Dir(src)))

	target := t.TempDir()
	var ie *IntegrityError
	if _, err := Restore(ctx, dest, target, RestoreOptions{}); !errors.As(err, &ie) || ie.Stage != StageRestore {
		t.Fatalf("restore of a corrupt backup: %v", err)
	}
	ents, _ := os.ReadDir(Dir(target))
	for _, e := range ents {
		if !lockFile(e.Name()) {
			t.Errorf("refused restore left %s in the target", e.Name())
		}
	}

	res, err := Restore(ctx, dest, target, RestoreOptions{AllowCorrupt: true})
	if err != nil {
		t.Fatalf("Restore with AllowCorrupt: %v", err)
	}
	if res.Verify.Health != HealthCorrupt {
		t.Errorf("health = %q", res.Verify.Health)
	}
	sameSnapshot(t, damaged, snapshot(t, Dir(target)))
	// It is still refused by the strict Open.
	if _, err := Open(target); !errors.Is(err, ErrIntegrity) {
		t.Errorf("Open of a restored corrupt database: %v", err)
	}
}
