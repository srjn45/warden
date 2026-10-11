package wardendb

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

// dropIndex removes a collection's primary index: a derived structure.
func dropIndex(t *testing.T, dataDir, collection string) {
	t.Helper()
	if err := os.Remove(filepath.Join(Dir(dataDir), collection, "index.json")); err != nil {
		t.Fatal(err)
	}
}

func TestRepairRequiresBackupDirectory(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	seed(t, dataDir)
	corruptFirstRecord(t, newestSegment(t, dataDir, "agents"))
	before := snapshot(t, Dir(dataDir))

	rec := &recorder{}
	if _, err := Repair(ctx, dataDir, RepairOptions{Salvage: true}, observed(rec)...); !errors.Is(err, ErrRepairBackupRequired) {
		t.Fatalf("no backup directory: %v", err)
	}
	if _, err := Repair(ctx, dataDir, RepairOptions{Salvage: true, BackupDir: filepath.Join(Dir(dataDir), "bk")}); !errors.Is(err, ErrRepairBackupRequired) {
		t.Fatalf("backup directory inside warden-db: %v", err)
	}
	sameSnapshot(t, before, snapshot(t, Dir(dataDir)))
	if rec.count(ActionRepairRefused, "") != 1 {
		t.Error("refusal was not audited")
	}
	if _, err := Repair(ctx, t.TempDir(), RepairOptions{BackupDir: t.TempDir()}); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing database: %v", err)
	}
}

func TestRepairRequiresExclusiveOwnership(t *testing.T) {
	dataDir, backups := t.TempDir(), t.TempDir()
	seed(t, dataDir)
	db := mustOpen(t, dataDir)

	res, err := Repair(context.Background(), dataDir, RepairOptions{BackupDir: backups, Salvage: true})
	if !errors.Is(err, ErrLocked) || res != nil {
		t.Fatalf("Repair of a live database = %+v, %v", res, err)
	}
	if got := dirEntries(t, backups); len(got) != 0 {
		t.Errorf("a refused repair wrote %v", got)
	}
	if n := agentCount(t, db); n != 3 {
		t.Errorf("owner reads %d records", n)
	}
}

func TestRepairLeavesAHealthyDatabaseAlone(t *testing.T) {
	dataDir, backups := t.TempDir(), t.TempDir()
	seed(t, dataDir)
	before := snapshot(t, Dir(dataDir))

	rec := &recorder{}
	res, err := Repair(context.Background(), dataDir, RepairOptions{BackupDir: backups, Salvage: true}, observed(rec)...)
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed || res.BackupDir != "" || res.After != nil || res.Before.Health != HealthHealthy {
		t.Fatalf("result = %+v", res)
	}
	if len(res.Collections) != 1 || res.Collections[0].Outcome != RepairUnchanged {
		t.Errorf("collections = %+v", res.Collections)
	}
	sameSnapshot(t, before, snapshot(t, Dir(dataDir)))
	if got := dirEntries(t, backups); len(got) != 0 {
		t.Errorf("an unchanged repair wrote %v", got)
	}
	if ev := rec.event(ActionRepair); ev == nil || ev.Detail["changed"] != "false" {
		t.Errorf("repair event = %+v", ev)
	}
	if st, err := Probe(dataDir); err != nil || st.Owned {
		t.Fatalf("Repair kept ownership: %+v, %v", st, err)
	}
}

func TestRepairRebuildsDerivedStructuresAfterBackup(t *testing.T) {
	ctx := context.Background()
	dataDir, backups := t.TempDir(), t.TempDir()
	seed(t, dataDir)
	dropIndex(t, dataDir, "agents")
	damaged := snapshot(t, Dir(dataDir))

	rec := &recorder{}
	res, err := Repair(ctx, dataDir, RepairOptions{BackupDir: backups}, observed(rec)...)
	if err != nil {
		t.Fatalf("Repair: %v", err)
	}
	if !res.Changed || res.Before.Health != HealthDegraded || res.After == nil || res.After.Health != HealthHealthy {
		t.Fatalf("result = %+v (before %q)", res, res.Before.Health)
	}
	c := res.Collections[0]
	if c.Name != "agents" || c.Outcome != RepairRepaired || len(c.Actions) == 0 || len(c.Classes) != 1 || c.Classes[0] != ClassDerived {
		t.Errorf("collection = %+v", c)
	}
	if len(c.Quarantined) != 0 || c.LostRegions != 0 {
		t.Errorf("a rebuild salvaged: %+v", c)
	}

	// The backup holds the directory exactly as it was before the repair.
	if res.Backup == nil || res.Backup.Kind != BackupCopy || !strings.HasPrefix(res.BackupDir, backups) {
		t.Fatalf("backup = %+v at %q", res.Backup, res.BackupDir)
	}
	if _, err := VerifyBackup(res.BackupDir); err != nil {
		t.Fatalf("pre-repair backup does not verify: %v", err)
	}
	sameSnapshot(t, damaged, snapshot(t, filepath.Join(res.BackupDir, BackupDataDir)))
	if res.EngineBackupDir == "" || inside(Dir(dataDir), res.EngineBackupDir) {
		t.Errorf("engine backup = %q", res.EngineBackupDir)
	}
	// No segment was edited.
	after := snapshot(t, Dir(dataDir))
	for name, b := range damaged {
		if strings.HasSuffix(name, ".ndjson") && string(after[name]) != string(b) {
			t.Errorf("segment %s was edited", name)
		}
	}

	if rec.count(ActionBackup, "") != 1 || rec.count(ActionRepair, "") != 1 || rec.count(ActionRepairRefused, "") != 0 {
		t.Errorf("events: backup=%d repair=%d refused=%d", rec.count(ActionBackup, ""), rec.count(ActionRepair, ""), rec.count(ActionRepairRefused, ""))
	}
	if ev := rec.event(ActionRepair); ev == nil || ev.Detail["changed"] != "true" || ev.Detail["collections"] != "agents" || ev.Detail["backup"] != res.BackupDir {
		t.Errorf("repair event = %+v", ev)
	}
	if b, err := json.Marshal(res); err != nil || strings.Contains(string(b), "agent-1") {
		t.Errorf("result JSON = %s, %v", b, err)
	}
	openVerified(t, dataDir)

	// Idempotent: a second run finds nothing to do.
	res, err = Repair(ctx, dataDir, RepairOptions{BackupDir: backups})
	if err != nil || res.Changed || res.BackupDir != "" {
		t.Errorf("second Repair = %+v, %v", res, err)
	}
}

func TestRepairRefusesSegmentDamageWithoutSalvage(t *testing.T) {
	dataDir, backups := t.TempDir(), t.TempDir()
	seed(t, dataDir)
	corruptFirstRecord(t, newestSegment(t, dataDir, "agents"))
	before := snapshot(t, Dir(dataDir))

	rec := &recorder{}
	res, err := Repair(context.Background(), dataDir, RepairOptions{BackupDir: backups}, observed(rec)...)
	if !errors.Is(err, ErrRepairRefused) {
		t.Fatalf("Repair err = %v, want ErrRepairRefused", err)
	}
	if res == nil || res.Changed || res.BackupDir != "" || len(res.Refused) == 0 {
		t.Fatalf("result = %+v", res)
	}
	if c := res.Collections[0]; c.Outcome != RepairRefused || c.Reason == "" {
		t.Errorf("collection = %+v", c)
	}
	sameSnapshot(t, before, snapshot(t, Dir(dataDir)))
	if got := dirEntries(t, backups); len(got) != 0 {
		t.Errorf("a refused repair wrote %v", got)
	}
	if rec.count(ActionRepairRefused, "") != 1 || rec.count(ActionRepair, "") != 0 || rec.count(ActionBackup, "") != 0 {
		t.Errorf("events: refused=%d repair=%d backup=%d", rec.count(ActionRepairRefused, ""), rec.count(ActionRepair, ""), rec.count(ActionBackup, ""))
	}
	// Still refused by the normal path.
	if _, err := Open(dataDir); !errors.Is(err, ErrIntegrity) {
		t.Errorf("Open err = %v, want ErrIntegrity", err)
	}
	sameSnapshot(t, before, snapshot(t, Dir(dataDir)))
}

func TestRepairRefusesWhenNoBackupCanBeTaken(t *testing.T) {
	dataDir := t.TempDir()
	seed(t, dataDir)
	corruptFirstRecord(t, newestSegment(t, dataDir, "agents"))
	before := snapshot(t, Dir(dataDir))

	// The backup location is a file, so nothing can be written under it.
	blocked := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := Repair(context.Background(), dataDir, RepairOptions{BackupDir: blocked, Salvage: true})
	if !errors.Is(err, ErrRepairRefused) || !errors.Is(err, ErrBackupDestination) {
		t.Fatalf("Repair err = %v", err)
	}
	if res == nil || res.Changed || res.BackupDir != "" {
		t.Fatalf("result = %+v", res)
	}
	sameSnapshot(t, before, snapshot(t, Dir(dataDir)))
}

func TestRepairSalvagesOnlyWhenAsked(t *testing.T) {
	ctx := context.Background()
	dataDir, backups := t.TempDir(), t.TempDir()
	seed(t, dataDir)
	seg := newestSegment(t, dataDir, "agents")
	corruptFirstRecord(t, seg)
	damaged := snapshot(t, Dir(dataDir))

	rec := &recorder{}
	res, err := Repair(ctx, dataDir, RepairOptions{BackupDir: backups, Salvage: true}, observed(rec)...)
	if err != nil {
		t.Fatalf("Repair: %v", err)
	}
	if !res.Changed || res.Before.Health != HealthCorrupt || res.After == nil || res.After.Blocking != 0 {
		t.Fatalf("result = %+v", res)
	}
	c := res.Collections[0]
	if c.Outcome != RepairRepaired || c.NewSegment == "" || len(c.Quarantined) == 0 || c.QuarantineDir == "" || c.LostRegions == 0 {
		t.Fatalf("collection = %+v", c)
	}
	// The damaged original is quarantined byte-for-byte, and backed up.
	q, err := os.ReadFile(filepath.Join(Dir(dataDir), "agents", filepath.FromSlash(c.QuarantineDir), filepath.Base(seg)))
	if err != nil {
		t.Fatalf("quarantined segment: %v", err)
	}
	rel, _ := filepath.Rel(Dir(dataDir), seg)
	if string(q) != string(damaged[rel]) {
		t.Error("quarantined segment differs from the damaged original")
	}
	sameSnapshot(t, damaged, snapshot(t, filepath.Join(res.BackupDir, BackupDataDir)))
	if rec.count(ActionBackup, "") != 1 || rec.count(ActionRepair, "") != 1 {
		t.Errorf("events: backup=%d repair=%d", rec.count(ActionBackup, ""), rec.count(ActionRepair, ""))
	}

	// The surviving records are served by a strict Open; the damaged one is gone.
	db := mustOpen(t, dataDir)
	if n := agentCount(t, db); n != 2 {
		t.Errorf("read %d records after salvage, want 2", n)
	}
	v, err := db.Verify(ctx)
	if err != nil || v.Blocking != 0 {
		t.Fatalf("verify after salvage = %+v, %v", v, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// The repair can be undone from its backup.
	if _, err := Restore(ctx, res.BackupDir, dataDir, RestoreOptions{Overwrite: true, AllowCorrupt: true}); err != nil {
		t.Fatalf("Restore of the pre-repair backup: %v", err)
	}
	sameSnapshot(t, damaged, snapshot(t, Dir(dataDir)))
}

func TestOpenForRepair(t *testing.T) {
	dataDir, backups := t.TempDir(), t.TempDir()
	seed(t, dataDir)
	corruptFirstRecord(t, newestSegment(t, dataDir, "agents"))
	before := snapshot(t, Dir(dataDir))

	// Without repair options, and without Salvage, the damage stays refused.
	if db, err := OpenForRepair(dataDir); db != nil || !errors.Is(err, ErrRepairBackupRequired) {
		t.Fatalf("OpenForRepair without WithRepair = %v, %v", db, err)
	}
	var seen *RepairResult
	note := func(r *RepairResult) { seen = r }
	if db, err := OpenForRepair(dataDir, WithRepair(RepairOptions{BackupDir: backups, OnResult: note})); db != nil || !errors.Is(err, ErrRepairRefused) {
		t.Fatalf("OpenForRepair without Salvage = %v, %v", db, err)
	}
	if seen == nil || len(seen.Refused) == 0 {
		t.Errorf("OnResult saw %+v", seen)
	}
	sameSnapshot(t, before, snapshot(t, Dir(dataDir)))
	// WithRepair does not make Open tolerant.
	if _, err := Open(dataDir, WithRepair(RepairOptions{BackupDir: backups, Salvage: true})); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("Open with WithRepair = %v, want ErrIntegrity", err)
	}
	sameSnapshot(t, before, snapshot(t, Dir(dataDir)))

	rec := &recorder{}
	opts := append(observed(rec), WithRepair(RepairOptions{BackupDir: backups, Salvage: true, OnResult: note}))
	db, err := OpenForRepair(dataDir, opts...)
	if err != nil {
		t.Fatalf("OpenForRepair: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if seen == nil || !seen.Changed || seen.BackupDir == "" {
		t.Errorf("OnResult saw %+v", seen)
	}
	if rec.count(ActionRepair, "") != 1 || rec.count(ActionOpen, "") != 1 {
		t.Errorf("events: repair=%d open=%d", rec.count(ActionRepair, ""), rec.count(ActionOpen, ""))
	}
	// The handle is an ordinary strict one.
	if n := agentCount(t, db); n != 2 {
		t.Errorf("read %d records, want 2", n)
	}
	if _, err := Open(dataDir); !errors.Is(err, ErrLocked) {
		t.Errorf("second Open = %v, want ErrLocked", err)
	}
}
