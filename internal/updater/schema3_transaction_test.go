package updater

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/srjn45/warden/internal/migrate/schema3"
	"github.com/srjn45/warden/internal/ownerlock"
	"github.com/srjn45/warden/internal/schema"
	"github.com/srjn45/warden/internal/wardendb"
	"github.com/stretchr/testify/require"
)

func TestVerifySchema3OwnershipAuthenticatesStartedTarget(t *testing.T) {
	ctx := context.Background()
	tr, old, target := schema3Fixture(t)
	self, err := os.Executable()
	require.NoError(t, err)
	bytes, err := os.ReadFile(self)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(target, bytes, 0o755))
	beginSchema3Fixture(t, tr, old, target)
	require.NoError(t, tr.promote(ctx))
	require.NoError(t, tr.handoff())
	require.NoError(t, VerifySchema3Ownership(ctx, tr.dir, tr.id, tr.manifest.TargetBinaryHash, tr.manifestHash))
	for _, mutate := range []func(){
		func() { tr.manifestHash = "sha256:wrong" },
		func() { tr.manifestHash = "" },
	} {
		mutate()
		require.Error(t, VerifySchema3Ownership(ctx, tr.dir, tr.id, tr.manifest.TargetBinaryHash, tr.manifestHash))
	}
}

func schema3Fixture(t *testing.T) (*schema3Transaction, string, string) {
	t.Helper()
	dir := t.TempDir()
	db, err := wardendb.OpenOfflineStagingDir(filepath.Join(dir, "warden-db"))
	require.NoError(t, err)
	col, err := db.Collection("projects")
	require.NoError(t, err)
	_, _, err = col.InsertWithKey("p", map[string]any{"id": "p", "name": "remote", "path": "", "agents": nil})
	require.NoError(t, err)
	col, err = db.Collection("agents")
	require.NoError(t, err)
	_, _, err = col.InsertWithKey("a", map[string]any{"id": "a", "project_id": "p", "prompt": "preserve full prompt", "child_agents": []any{}})
	require.NoError(t, err)
	require.NoError(t, db.Close())
	require.NoError(t, schema.Save(dir, &schema.Ledger{SchemaVersion: 2, BinaryVersion: "old-test", History: []schema.HistoryEntry{}}))
	old, newBin := filepath.Join(t.TempDir(), "warden"), filepath.Join(t.TempDir(), "target")
	require.NoError(t, os.WriteFile(old, []byte("old binary"), 0755))
	require.NoError(t, os.WriteFile(newBin, []byte("target binary"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("setting: original\n"), 0600))
	tr, err := schema3Path(dir, "fixture")
	require.NoError(t, err)
	tr.lock, err = ownerlock.Acquire(dir, ownerlock.Info{Kind: ownerlock.KindCLI})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, tr.close()) })
	return tr, old, newBin
}
func beginSchema3Fixture(t *testing.T, tr *schema3Transaction, old, target string) {
	t.Helper()
	require.NoError(t, tr.begin(context.Background(), old, target, "old-test", "target-test", []string{"config.yaml"}, 1<<50))
}

func TestSchema3ReadinessFinalizationResumes(t *testing.T) {
	points := []string{
		"before/readiness_verified/intent", "after/readiness_verified/intent",
		"before/readiness-ledger-save", "after/readiness-ledger-save",
		"before/readiness_verified/complete", "after/readiness_verified/complete",
	}
	for _, point := range points {
		t.Run(point, func(t *testing.T) {
			ctx := context.Background()
			tr, old, target := schema3Fixture(t)
			beginSchema3Fixture(t, tr, old, target)
			require.NoError(t, tr.promote(ctx))
			tr.fault = func(p string) error {
				if p == point {
					return errors.New("finalization interrupted")
				}
				return nil
			}
			health := Health{Status: "ok", Version: "target-test", SchemaVersion: 3}
			require.Error(t, tr.finish(ctx, health, true))
			tr.fault = nil
			require.NoError(t, tr.load(target))
			require.NoError(t, tr.promote(ctx))
			require.NoError(t, tr.finish(ctx, health, true))
			require.True(t, tr.done("readiness_verified"))
			l, err := schema.Load(tr.dir)
			require.NoError(t, err)
			require.Nil(t, l.InProgress)
			require.Len(t, l.History, 1, "resuming must not repeat the migration")
			count := len(tr.records)
			require.NoError(t, tr.finish(ctx, health, true))
			require.Len(t, tr.records, count, "finalization is an idempotent no-op")
		})
	}
}

func TestSchema3FinalizationBindsWholeLedgerTransaction(t *testing.T) {
	for _, field := range []string{"transaction", "manifest", "journal", "binary", "source", "target", "step"} {
		t.Run(field, func(t *testing.T) {
			ctx := context.Background()
			tr, old, target := schema3Fixture(t)
			beginSchema3Fixture(t, tr, old, target)
			require.NoError(t, tr.promote(ctx))
			l, err := schema.Load(tr.dir)
			require.NoError(t, err)
			switch field {
			case "transaction":
				l.InProgress.TransactionID = "unrelated"
			case "manifest":
				l.InProgress.ManifestSHA256 = "unrelated"
			case "journal":
				l.InProgress.Journal = "unrelated"
			case "binary":
				l.InProgress.TargetBinarySHA256 = "unrelated"
			case "source":
				l.InProgress.SourceFormat = 1
			case "target":
				l.InProgress.TargetFormat = 4
			case "step":
				l.InProgress.Step = "unrelated"
			}
			require.NoError(t, schema.Save(tr.dir, l))
			require.ErrorContains(t, tr.finish(ctx, Health{Status: "ok", Version: "target-test", SchemaVersion: 3}, true), "transaction mismatch")
		})
	}
}

func TestSchema3PromotionRejectsForeignLedgerBeforeGenerationMutation(t *testing.T) {
	ctx := context.Background()
	tr, old, target := schema3Fixture(t)
	beginSchema3Fixture(t, tr, old, target)
	before, err := schema3Tree(filepath.Join(tr.dir, "warden-db"))
	require.NoError(t, err)
	l, err := schema.Load(tr.dir)
	require.NoError(t, err)
	l.InProgress.TransactionID = "another-update"
	require.NoError(t, schema.Save(tr.dir, l))
	require.ErrorContains(t, tr.promote(ctx), "conflicting ledger transaction")
	after, err := schema3Tree(filepath.Join(tr.dir, "warden-db"))
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.NoDirExists(t, tr.stage)
	require.NoDirExists(t, tr.park)
}

func TestSchema3RestoredTransactionCannotPromoteOrDiscardLaterWrites(t *testing.T) {
	ctx := context.Background()
	tr, old, target := schema3Fixture(t)
	beginSchema3Fixture(t, tr, old, target)
	require.NoError(t, tr.promote(ctx))
	require.NoError(t, tr.restore(ctx, old))
	require.NoError(t, tr.load(target))
	require.NoError(t, tr.restore(ctx, old))
	require.ErrorContains(t, tr.promote(ctx), "selected for restore")
	path := filepath.Join(tr.dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("setting: later old-daemon write\n"), 0600))
	require.ErrorContains(t, tr.restore(ctx, old), "changed; refusing replay")
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "setting: later old-daemon write\n", string(got))
}
func TestSchema3BackupPromotionAndPairedRestore(t *testing.T) {
	ctx := context.Background()
	tr, old, target := schema3Fixture(t)
	before, err := schema3Tree(filepath.Join(tr.dir, "warden-db"))
	require.NoError(t, err)
	ledger, err := os.ReadFile(filepath.Join(tr.dir, schema.FileName))
	require.NoError(t, err)
	beginSchema3Fixture(t, tr, old, target)
	require.NoError(t, tr.verifyBundle())
	original, err := os.ReadFile(filepath.Join(tr.bundle, "data", schema.FileName))
	require.NoError(t, err)
	require.Equal(t, ledger, original)
	require.NoError(t, tr.promote(ctx))
	l, err := schema.Load(tr.dir)
	require.NoError(t, err)
	require.Equal(t, 3, l.SchemaVersion)
	require.NotNil(t, l.InProgress, "ordinary startup must refuse even with format 3 ledger")
	g, _, err := schema3.Read(ctx, filepath.Join(tr.dir, "warden-db"))
	require.NoError(t, err)
	require.Contains(t, g, "plan_tasks")
	require.NotContains(t, g["agents"]["a"], "child_agents")
	require.Error(t, tr.finish(ctx, Health{Status: "ok", Version: "wrong", SchemaVersion: 3}, true))
	require.NoError(t, os.WriteFile(old, []byte("target binary"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(tr.dir, "config.yaml"), []byte("new config"), 0600))
	require.NoError(t, tr.restore(ctx, old))
	after, err := schema3Tree(filepath.Join(tr.dir, "warden-db"))
	require.NoError(t, err)
	require.Equal(t, before, after)
	b, err := os.ReadFile(old)
	require.NoError(t, err)
	require.Equal(t, "old binary", string(b))
	b, err = os.ReadFile(filepath.Join(tr.dir, schema.FileName))
	require.NoError(t, err)
	require.Equal(t, ledger, b)
	_, err = os.Stat(filepath.Join(tr.dir, "warden-db", "plan_tasks"))
	require.True(t, os.IsNotExist(err))
	require.NoError(t, tr.restore(ctx, old), "repeat restore idempotent")
}
func TestSchema3PromotionFaultsResume(t *testing.T) {
	points := []string{"before/staging-write", "after/staging-write", "before/park-rename", "after/park-rename", "before/promote-rename", "after/promote-rename", "before/ledger-save", "after/ledger-save"}
	for _, step := range []string{"staged", "verified", "old_generation_parked", "promoted", "ledger_committed"} {
		for _, phase := range []string{"intent", "complete"} {
			points = append(points, "before/"+step+"/"+phase, "after/"+step+"/"+phase)
		}
	}
	for _, point := range points {
		t.Run(point, func(t *testing.T) {
			ctx := context.Background()
			tr, old, target := schema3Fixture(t)
			beginSchema3Fixture(t, tr, old, target)
			injected := false
			tr.fault = func(p string) error {
				if p == point && !injected {
					injected = true
					return errors.New("injected crash")
				}
				return nil
			}
			require.Error(t, tr.promote(ctx))
			require.True(t, injected)
			tr.fault = nil
			require.NoError(t, tr.load(target))
			require.NoError(t, tr.promote(ctx))
			require.NoError(t, tr.verifyTarget(ctx, filepath.Join(tr.dir, "warden-db")))
			require.NoError(t, tr.promote(ctx))
			require.NoError(t, tr.restore(ctx, old))
		})
	}
}
func TestSchema3BackupFaultsLeaveOriginalUntouched(t *testing.T) {
	for _, point := range []string{"before/backup-create", "before/backup/warden-db", "after/backup/warden-db", "before/backup/schema.json", "after/backup/schema.json", "before/backup/config.yaml", "after/backup/config.yaml", "before/backup/binary", "after/backup/binary", "before/manifest", "after/manifest"} {
		t.Run(point, func(t *testing.T) {
			tr, old, target := schema3Fixture(t)
			before, err := schema3Tree(filepath.Join(tr.dir, "warden-db"))
			require.NoError(t, err)
			l, err := os.ReadFile(filepath.Join(tr.dir, schema.FileName))
			require.NoError(t, err)
			tr.fault = func(p string) error {
				if p == point {
					return errors.New("disk failure")
				}
				return nil
			}
			require.Error(t, tr.begin(context.Background(), old, target, "old-test", "target-test", []string{"config.yaml"}, 1<<50))
			after, err := schema3Tree(filepath.Join(tr.dir, "warden-db"))
			require.NoError(t, err)
			require.Equal(t, before, after)
			got, err := os.ReadFile(filepath.Join(tr.dir, schema.FileName))
			require.NoError(t, err)
			require.Equal(t, l, got)
			tr.fault = nil
			require.NoError(t, tr.load(target))
			require.NoError(t, tr.promote(context.Background()))
			require.NoError(t, tr.restore(context.Background(), old))
		})
	}
}
func TestSchema3RestoreFaultsResume(t *testing.T) {
	for _, point := range []string{"before/restore-park", "after/restore-park", "before/restore-promote", "after/restore-promote", "before/restore/schema.json", "after/restore/schema.json", "before/restore/config.yaml", "after/restore/config.yaml", "before/restore-binary", "after/restore-binary"} {
		t.Run(point, func(t *testing.T) {
			tr, old, target := schema3Fixture(t)
			beginSchema3Fixture(t, tr, old, target)
			require.NoError(t, tr.promote(context.Background()))
			tr.fault = func(p string) error {
				if p == point {
					return errors.New("restore interrupted")
				}
				return nil
			}
			require.Error(t, tr.restore(context.Background(), old))
			tr.fault = nil
			require.NoError(t, tr.load(target))
			require.NoError(t, tr.restore(context.Background(), old))
			l, err := schema.Load(tr.dir)
			require.NoError(t, err)
			require.Equal(t, 2, l.SchemaVersion)
		})
	}
}
func TestSchema3TamperAndIdentityGuards(t *testing.T) {
	for _, what := range []string{"binary", "backup", "staging", "journal", "empty journal", "unterminated journal", "manifest", "source", "disk"} {
		t.Run(what, func(t *testing.T) {
			tr, old, target := schema3Fixture(t)
			if what == "disk" {
				require.ErrorContains(t, tr.begin(context.Background(), old, target, "old-test", "target-test", nil, 0), "space")
				return
			}
			beginSchema3Fixture(t, tr, old, target)
			switch what {
			case "binary":
				require.NoError(t, os.WriteFile(target, []byte("wrong"), 0755))
				require.ErrorContains(t, tr.load(target), "binary")
			case "backup":
				require.NoError(t, os.WriteFile(filepath.Join(tr.bundle, "data", "config.yaml"), []byte("tamper"), 0600))
				require.Error(t, tr.verifyBundle())
			case "manifest":
				require.NoError(t, os.Chmod(filepath.Join(tr.bundle, "MANIFEST.json"), 0600))
				require.NoError(t, os.WriteFile(filepath.Join(tr.bundle, "MANIFEST.json"), []byte("{}"), 0400))
				require.Error(t, tr.verifyBundle())
			case "journal":
				f, err := os.OpenFile(tr.journal, os.O_APPEND|os.O_WRONLY, 0600)
				require.NoError(t, err)
				_, err = f.WriteString("{torn\n")
				require.NoError(t, err)
				require.NoError(t, f.Close())
				require.Error(t, tr.load(target))
			case "empty journal":
				require.NoError(t, os.WriteFile(tr.journal, nil, 0600))
				require.ErrorContains(t, tr.load(target), "empty/unsafe journal")
			case "unterminated journal":
				b, err := os.ReadFile(tr.journal)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(tr.journal, b[:len(b)-1], 0600))
				require.ErrorContains(t, tr.load(target), "unterminated journal")
			case "staging":
				require.NoError(t, os.Mkdir(tr.stage, 0700))
				require.Error(t, tr.promote(context.Background()))
			case "source":
				require.NoError(t, os.WriteFile(filepath.Join(tr.dir, "warden-db", "unexpected"), []byte("change"), 0600))
				require.Error(t, tr.promote(context.Background()))
			}
		})
	}
}
func TestSchema3BundlesNeverGenericRollbackOrPrune(t *testing.T) {
	dir := t.TempDir()
	bundle := filepath.Join(dir, "backups", schema3BackupPrefix+"test")
	require.NoError(t, os.MkdirAll(bundle, 0700))
	_, err := LatestSnapshot(dir)
	require.ErrorIs(t, err, ErrNoSnapshot)
	entries, err := os.ReadDir(filepath.Dir(bundle))
	require.NoError(t, err)
	require.False(t, isUpdaterSnapshotDir(entries[0]))
}
func TestSchema3ManifestAndReportExample(t *testing.T) {
	tr, old, target := schema3Fixture(t)
	beginSchema3Fixture(t, tr, old, target)
	raw, err := json.MarshalIndent(tr.manifest, "", "  ")
	require.NoError(t, err)
	require.Contains(t, string(raw), `"source_format": 2`)
	require.Contains(t, string(raw), `"target_format": 3`)
	require.Contains(t, string(raw), `"restore_deletion_set"`)
}

func TestSchema3ReadinessAndRepeatedUpdateAreIntegrityNoop(t *testing.T) {
	ctx := context.Background()
	tr, old, target := schema3Fixture(t)
	beginSchema3Fixture(t, tr, old, target)
	require.NoError(t, tr.promote(ctx))
	require.Error(t, tr.finish(ctx, Health{Status: "ok", Version: "target-test", SchemaVersion: 2}, true))
	require.Error(t, tr.finish(ctx, Health{Status: "ok", Version: "target-test", SchemaVersion: 3}, false))
	require.NoError(t, tr.finish(ctx, Health{Status: "ok", Version: "target-test", SchemaVersion: 3}, true))
	before, err := os.ReadFile(tr.journal)
	require.NoError(t, err)
	require.NoError(t, tr.load(target))
	require.NoError(t, tr.promote(ctx))
	after, err := os.ReadFile(tr.journal)
	require.NoError(t, err)
	require.Equal(t, before, after)
	l, err := schema.Load(tr.dir)
	require.NoError(t, err)
	require.Nil(t, l.InProgress)
}
func TestSchema3ManagedStopMustBeConfirmed(t *testing.T) {
	for _, what := range []string{"unmanaged", "no manager", "unknown manager", "stop failure", "still running", "supervisor active", "health still alive", "stopped"} {
		t.Run(what, func(t *testing.T) {
			tr, err := schema3Path(t.TempDir(), "stoptest")
			require.NoError(t, err)
			svc := &fakeSvc{state: ServiceState{Kind: ServiceSystemd}, exited: func(int) bool { return true }}
			probe := &fakeProbe{}
			switch what {
			case "unmanaged":
				svc.state.Kind = ServiceManual
			case "no manager":
				svc.state.Kind = ServiceNone
			case "unknown manager":
				svc.state.Kind = ServiceKind("unrecognized")
			case "stop failure":
				svc.stopErr = func() error { return errors.New("failed stop") }
			case "still running":
				svc.exited = func(int) bool { return false }
			case "supervisor active":
				svc.state.Active = true
			case "health still alive":
				probe.fn = func() (Health, error) { return Health{Status: "ok"}, nil }
			}
			err = acquireSchema3(context.Background(), tr, svc, probe)
			if what == "stopped" {
				require.NoError(t, err)
				require.NotNil(t, tr.lock)
				require.NoError(t, tr.close())
			} else {
				require.Error(t, err)
				require.Nil(t, tr.lock)
			}
		})
	}
}
