package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/audit"
	"github.com/srjn45/warden/internal/migrate"
	"github.com/srjn45/warden/internal/ownerlock"
	"github.com/srjn45/warden/internal/schema"
	"github.com/srjn45/warden/internal/storage"
	"github.com/srjn45/warden/internal/store"
)

// legacyDataDir builds a schema-1 data dir through the real compat stores: one
// LIVE agent (must come out untouched), a project, and an audit event. Temp dir
// only — nothing here ever touches a real ~/.warden.
func legacyDataDir(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	require.NoError(t, schema.Save(d, &schema.Ledger{SchemaVersion: 1, BinaryVersion: "10.0.1", History: []schema.HistoryEntry{}}))
	s, err := storage.OpenLegacy(d)
	require.NoError(t, err)
	now := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	_, err = s.Projects.OpenProject("/work/app", "app", "/work/app")
	require.NoError(t, err)
	require.NoError(t, s.Agents.Create(context.Background(), &agentstore.Agent{ID: "agent-live", Name: "live", Type: store.Type("worker"),
		TmuxSession: "warden-live", Repo: "/work/app", ProjectID: "/work/app", Status: store.StatusWorking, PID: 4242, CreatedAt: now, UpdatedAt: now}))
	s.Audit.Log(audit.Event{Time: now, Action: "spawn", Target: "agent-live"})
	require.NoError(t, s.Close())
	return d
}

// realMigrate stands in for `<new binary> migrate --apply`: it runs the real
// journaled cutover in-process, exactly as the new binary's command does.
func realMigrate(t *testing.T) func(context.Context, string, string, io.Writer) error {
	return func(ctx context.Context, bin, dataDir string, out io.Writer) error {
		lock, err := ownerlock.Acquire(dataDir, ownerlock.Info{Kind: ownerlock.KindCLI, Version: "11.0.0", Command: "test migrate --apply"})
		if err != nil {
			return err
		}
		defer lock.Release()
		return migrate.NewRunner(nil).Apply(migrate.Env{DataDir: dataDir, BinaryVersion: "11.0.0", Context: ctx}, schema.Schema2)
	}
}

func stubUpdateSeams(t *testing.T, target int, run func(context.Context, string, string, io.Writer) error) *int {
	t.Helper()
	oldSchema, oldRun := targetBinarySchema, runTargetMigrate
	calls := 0
	targetBinarySchema = func(context.Context, string) (int, error) { return target, nil }
	runTargetMigrate = func(ctx context.Context, bin, dir string, out io.Writer) error {
		calls++
		return run(ctx, bin, dir, out)
	}
	t.Cleanup(func() { targetBinarySchema, runTargetMigrate = oldSchema, oldRun })
	return &calls
}

func TestUpdateMigrationsRunCutoverWithTargetBinary(t *testing.T) {
	d := legacyDataDir(t)
	calls := stubUpdateSeams(t, schema.Schema2, realMigrate(t))
	cfg := filepath.Join(t.TempDir(), "config.yaml")

	require.NoError(t, runUpdateMigrations(context.Background(), cfg, d, "/fake/warden", io.Discard))
	require.Equal(t, 1, *calls)

	l, err := schema.Load(d)
	require.NoError(t, err)
	require.Equal(t, schema.Schema2, l.SchemaVersion)
	require.Nil(t, l.InProgress)
	require.Equal(t, schema.VerdictStart, schema.Decide(l.SchemaVersion, schema.Schema2, 1, false), "the historical target binary accepts what the updater left")

	// Schema-2 daemon view: live agent intact, audit history readable.
	s, err := storage.OpenSchema2ForMigrationVerification(d)
	require.NoError(t, err)
	defer s.Close()
	a, err := s.Agents.Get(context.Background(), "agent-live")
	require.NoError(t, err)
	require.Equal(t, store.StatusWorking, a.Status)
	evs, err := s.Audit.Read(audit.Filter{})
	require.NoError(t, err)
	require.Len(t, evs, 1)

	// Second run (e.g. `wd update --force`) is a no-op: migrate exactly once.
	require.NoError(t, runUpdateMigrations(context.Background(), cfg, d, "/fake/warden", io.Discard))
	require.Equal(t, 1, *calls)
}

func TestUpdateMigrationsFailureSurfacesAndLeavesLegacyRecoverable(t *testing.T) {
	d := legacyDataDir(t)
	require.NoError(t, os.WriteFile(filepath.Join(d, "audit.jsonl"), []byte("{not json\n"), 0o600)) // corrupt flat source
	stubUpdateSeams(t, schema.Schema2, realMigrate(t))

	err := runUpdateMigrations(context.Background(), filepath.Join(t.TempDir(), "c.yaml"), d, "/fake/warden", io.Discard)
	require.Error(t, err)
	require.Contains(t, err.Error(), "previous binary and data are restored")
	require.False(t, schema.WardenDBPresent(d), "a refused cutover promotes nothing")
	l, lerr := schema.Load(d)
	require.NoError(t, lerr)
	require.Equal(t, 1, l.SchemaVersion)
	require.DirExists(t, filepath.Join(d, "agents-db"))
}

func TestUpdateMigrationsRefuseWhenLedgerNotAdvanced(t *testing.T) {
	d := legacyDataDir(t)
	stubUpdateSeams(t, schema.Schema2, func(context.Context, string, string, io.Writer) error { return nil }) // "succeeds", changes nothing
	err := runUpdateMigrations(context.Background(), filepath.Join(t.TempDir(), "c.yaml"), d, "/fake/warden", io.Discard)
	require.ErrorContains(t, err, "ledger is at schema 1")
}

func TestUpdateMigrationsSkips(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "c.yaml")
	for name, prep := range map[string]func() string{
		"already at target": func() string {
			d := t.TempDir()
			require.NoError(t, schema.Save(d, &schema.Ledger{SchemaVersion: 2}))
			return d
		},
		"fresh dir": func() string { return t.TempDir() },
	} {
		calls := stubUpdateSeams(t, schema.Schema2, func(context.Context, string, string, io.Writer) error { return errors.New("must not run") })
		require.NoError(t, runUpdateMigrations(context.Background(), cfg, prep(), "/fake/warden", io.Discard), name)
		require.Zero(t, *calls, name)
	}
	// A target that predates the ledger (reports 0) is never migrated by us.
	d := legacyDataDir(t)
	calls := stubUpdateSeams(t, 0, func(context.Context, string, string, io.Writer) error { return errors.New("must not run") })
	require.NoError(t, runUpdateMigrations(context.Background(), cfg, d, "/fake/warden", io.Discard))
	require.Zero(t, *calls)
}

func TestUpdateMigrationsRefuseInterruptedJournal(t *testing.T) {
	d := legacyDataDir(t)
	l, err := schema.Load(d)
	require.NoError(t, err)
	l.InProgress = &schema.InProgress{Migration: migrate.Migration0002ID, Step: "promoting"}
	require.NoError(t, schema.Save(d, l))
	calls := stubUpdateSeams(t, schema.Schema2, realMigrate(t))
	err = runUpdateMigrations(context.Background(), filepath.Join(t.TempDir(), "c.yaml"), d, "/fake/warden", io.Discard)
	require.ErrorContains(t, err, "was interrupted")
	require.Zero(t, *calls)
}

// The target binary's `migrate --check` (which the updater runs BEFORE any
// change) and its boot guard must tell the same story at every stage.
func TestSchema2CheckAndBootGuardAgree(t *testing.T) {
	d := legacyDataDir(t)
	check := func() migrateCheckReport {
		cmd := newMigrateCmd()
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		cmd.SetArgs([]string{"--check", "--target", "2", "--json", "--data-dir", d})
		_ = cmd.ExecuteContext(context.Background())
		var rep migrateCheckReport
		require.NoError(t, json.Unmarshal(buf.Bytes(), &rep), buf.String())
		return rep
	}

	// Before: schema 1. Check is NOT clean (so an updater that cannot run the
	// cutover blocks before it swaps anything), carries the pending marker, and
	// the boot guard refuses the same data.
	rep := check()
	require.False(t, rep.Clean)
	var pending bool
	for _, f := range rep.Findings {
		if f.Class == migrate.ClassSchema2CutoverPending {
			pending = true
			require.NotEmpty(t, f.Command)
		}
		require.NotEqual(t, migrate.SeverityBlocking, f.Severity, "a healthy schema-1 dir has no blockers: %+v", f)
	}
	require.True(t, pending)
	_, bootErr := schema.Boot(d, "11.0.0")
	var ge *schema.GuardError
	require.ErrorAs(t, bootErr, &ge)
	require.Equal(t, schema.VerdictNeedsMigration, ge.Verdict)

	// This updater turns the marker into a note, never a blocker.
	res := findingsToPreflightResult(rep.Findings)
	require.Empty(t, res.Blockers, "%v", res.Blockers)
	require.NotEmpty(t, res.Notes)

	// A genuinely corrupt source still blocks.
	bad := findingsToPreflightResult([]migrate.Finding{{Severity: migrate.SeverityBlocking, Class: string(migrate.ClassCorruption), Store: "context", Message: "torn"}})
	require.NotEmpty(t, bad.Blockers)

	// After the cutover: check is clean and the guard starts.
	stubUpdateSeams(t, schema.Schema2, realMigrate(t))
	require.NoError(t, runUpdateMigrations(context.Background(), filepath.Join(t.TempDir(), "c.yaml"), d, "/fake/warden", io.Discard))
	rep = check()
	require.True(t, rep.Clean, "%+v", rep.Findings)
	// This test models the released schema-2 target. The current schema-3
	// runtime correctly refuses a schema-2 generation.
	require.Equal(t, schema.VerdictStart, schema.Decide(schema.Schema2, schema.Schema2, 1, false))
}

// On schema 2 the schema-1 store directories are inert copies. CLI paths that
// would open them directly must refuse instead of silently reading frozen data
// or writing a store the daemon never looks at (a second writer).
func TestSchema2RefusesDirectLegacyStoreAccess(t *testing.T) {
	d := t.TempDir()
	require.NoError(t, schema.Save(d, &schema.Ledger{SchemaVersion: schema.Schema2}))
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(cfg, []byte("data_dir: "+d+"\n"), 0o600))

	for _, sub := range []string{"agents", "backends", "sessions", "all"} {
		cmd := newRepairCmd()
		cmd.PersistentFlags().String("config", "", "")
		cmd.SetArgs([]string{sub, "--config", cfg})
		cmd.SilenceErrors, cmd.SilenceUsage = true, true
		err := cmd.ExecuteContext(context.Background())
		require.Error(t, err, sub)
		require.Contains(t, err.Error(), "schema 2", sub)
	}

	require.Contains(t, errLegacyStoreInert("x").Error(), "repair warden-db")
	require.NoError(t, requireSchema1Stores(t.TempDir(), "x"), "fresh/legacy dirs are unaffected")
	res := checkBackendRegistry(context.Background(), d)
	require.True(t, res.ok)
	require.Contains(t, res.detail, "warden-db")
	require.NoDirExists(t, filepath.Join(d, "backends"), "a refused/skipped check must not create legacy stores")
}
