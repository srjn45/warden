package migrate

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/adoption"
	"github.com/srjn45/warden/internal/schema"
	"github.com/srjn45/warden/internal/storage"
)

func cutOver(t *testing.T) (string, *schema.Ledger) {
	t.Helper()
	dir := t.TempDir()
	seedRealisticLegacy(t, dir)
	env := Env{DataDir: dir, BinaryVersion: "11.0.0", Context: context.Background()}
	require.NoError(t, NewRunner(DefaultRegistry).Apply(env, schema.Schema2))
	l, err := schema.Load(dir)
	require.NoError(t, err)
	return dir, l
}

func recordBoots(t *testing.T, dir string, n int) {
	for i := 0; i < n; i++ {
		require.NoError(t, adoption.Record(dir, adoption.Event{Kind: adoption.RuntimeBoot, ToSchema: 2}))
	}
}

func crit(r *RetirementReport, id string) Criterion {
	for _, c := range r.Criteria {
		if c.ID == id {
			return c
		}
	}
	return Criterion{ID: "missing:" + id}
}

func TestRetirement_CutoverEmitsTelemetryAndEligibleAfterWindow(t *testing.T) {
	dir, l := cutOver(t)
	log, err := adoption.Read(dir)
	require.NoError(t, err)
	ev, ok := log.Last(adoption.MigrationApplied)
	require.True(t, ok, "a committed cutover must emit migration_applied")
	require.Equal(t, Migration0002ID, ev.Migration)
	require.Equal(t, 1, ev.FromSchema)
	require.Equal(t, 2, ev.ToSchema)
	require.Equal(t, l.History[0].Backup, ev.Backup)

	recordBoots(t, dir, 3)
	cut := l.History[0].At

	// Inside the window: only the window (not the evidence) blocks.
	early, err := EvaluateRetirement(dir, RetirementOptions{Now: cut.Add(24 * time.Hour)})
	require.NoError(t, err)
	require.False(t, early.Eligible)
	require.Equal(t, []string{CritSupportWindow}, early.Blockers)

	late, err := EvaluateRetirement(dir, RetirementOptions{Now: cut.Add(DefaultSupportWindow + time.Hour)})
	require.NoError(t, err)
	require.True(t, late.Eligible, late.Summary())
	require.Empty(t, late.Blockers)
	require.NotEmpty(t, late.LegacyPresent, "legacy stores are kept and reported, never deleted")
	require.Equal(t, 1, late.Counts[string(adoption.MigrationApplied)])
	require.Contains(t, late.Summary(), "ELIGIBLE")
}

func TestRetirement_NotEligibleBeforeCutover(t *testing.T) {
	dir := t.TempDir()
	seedRealisticLegacy(t, dir)
	rep, err := EvaluateRetirement(dir, RetirementOptions{Now: time.Now().Add(1000 * time.Hour)})
	require.NoError(t, err)
	require.False(t, rep.Eligible)
	require.Contains(t, rep.Blockers, CritSchema2Active)
	require.Contains(t, rep.Blockers, CritMigrationOK)
	require.Contains(t, rep.Blockers, CritWardenDBPresent)
}

func TestRetirement_TamperedBackupBlocks(t *testing.T) {
	dir, l := cutOver(t)
	recordBoots(t, dir, 3)
	backup := filepath.Join(dir, filepath.FromSlash(l.History[0].Backup), schema2BackupData)
	var victim string
	require.NoError(t, filepath.WalkDir(backup, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && victim == "" {
			victim = p
		}
		return err
	}))
	require.NotEmpty(t, victim)
	require.NoError(t, os.Chmod(victim, 0o600))
	require.NoError(t, os.WriteFile(victim, []byte("tampered"), 0o600))

	rep, err := EvaluateRetirement(dir, RetirementOptions{Now: l.History[0].At.Add(2 * DefaultSupportWindow)})
	require.NoError(t, err)
	require.False(t, rep.Eligible)
	require.False(t, crit(rep, CritBackupVerified).Met)
}

func TestRetirement_LegacyOpenAttemptIsRecordedAndBlocks(t *testing.T) {
	dir, l := cutOver(t)
	recordBoots(t, dir, 3)
	_, err := storage.OpenLegacy(dir)
	require.Error(t, err, "legacy stores are never a fallback after cutover")

	log, err := adoption.Read(dir)
	require.NoError(t, err)
	require.Equal(t, 1, log.Count(adoption.LegacyOpenRefused, time.Time{}))

	rep, err := EvaluateRetirement(dir, RetirementOptions{Now: l.History[0].At.Add(2 * DefaultSupportWindow)})
	require.NoError(t, err)
	require.Equal(t, []string{CritNoLegacyOpens}, rep.Blockers)
}

func TestRetirement_TooFewBootsBlocks(t *testing.T) {
	dir, l := cutOver(t)
	recordBoots(t, dir, 1)
	rep, err := EvaluateRetirement(dir, RetirementOptions{Now: l.History[0].At.Add(2 * DefaultSupportWindow)})
	require.NoError(t, err)
	require.Equal(t, []string{CritRuntimeBoots}, rep.Blockers)
}

func TestRetirement_RollbackAndFailureBlock(t *testing.T) {
	dir, l := cutOver(t)
	recordBoots(t, dir, 3)
	after := l.History[0].At.Add(time.Minute)
	require.NoError(t, adoption.Record(dir, adoption.Event{At: after, Kind: adoption.UpdaterRollback}))
	require.NoError(t, adoption.Record(dir, adoption.Event{At: after, Kind: adoption.MigrationFailed, Error: "boom"}))
	rep, err := EvaluateRetirement(dir, RetirementOptions{Now: l.History[0].At.Add(2 * DefaultSupportWindow)})
	require.NoError(t, err)
	require.ElementsMatch(t, []string{CritNoRollback, CritNoUnresolved}, rep.Blockers)
}

func TestRetirement_AbandonedTargetCountsAsRollback(t *testing.T) {
	dir, l := cutOver(t)
	recordBoots(t, dir, 3)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "backups", "abandoned-schema2-x"), 0o700))
	rep, err := EvaluateRetirement(dir, RetirementOptions{Now: l.History[0].At.Add(2 * DefaultSupportWindow)})
	require.NoError(t, err)
	require.Equal(t, []string{CritNoRollback}, rep.Blockers)
}

func TestRetirement_RefusedCutoverAndRestoreEmitTelemetry(t *testing.T) {
	dir := t.TempDir()
	seedRealisticLegacy(t, dir)
	rn := NewRunner(DefaultRegistry)
	env := Env{DataDir: dir, BinaryVersion: "11.0.0", Context: context.Background()}

	// Block the cutover: an unproven warden-db residue the cutover must not adopt.
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "warden-db"), 0o700))
	require.Error(t, rn.Apply(env, schema.Schema2))
	require.NoError(t, os.RemoveAll(filepath.Join(dir, "warden-db")))
	log, err := adoption.Read(dir)
	require.NoError(t, err)
	require.Empty(t, log.Events, "a pure refusal writes nothing, evidence log included")

	require.NoError(t, rn.Apply(env, schema.Schema2))
	done, err := schema.Load(dir)
	require.NoError(t, err)
	backup := done.History[len(done.History)-1].Backup
	require.NoError(t, schema.Save(dir, &schema.Ledger{SchemaVersion: 1, BinaryVersion: "10.0.1",
		InProgress: &schema.InProgress{Migration: Migration0002ID, Step: "promoted", Snapshot: backup}}))
	require.NoError(t, rn.Restore(env))
	log, err = adoption.Read(dir)
	require.NoError(t, err)
	r, ok := log.Last(adoption.RecoveryRestored)
	require.True(t, ok)
	require.Equal(t, Migration0002ID, r.Migration)
	require.Equal(t, backup, r.Backup)
}

func TestRetirement_ResumeEmitsRecovery(t *testing.T) {
	dir := t.TempDir()
	seedRealisticLegacy(t, dir)
	rn := NewRunner(DefaultRegistry)
	env := Env{DataDir: dir, BinaryVersion: "11.0.0", Context: context.Background()}
	require.NoError(t, rn.Apply(env, schema.Schema2))
	done, _ := schema.Load(dir)
	backup := done.History[len(done.History)-1].Backup
	require.NoError(t, schema.Save(dir, &schema.Ledger{SchemaVersion: 1, BinaryVersion: "10.0.1",
		InProgress: &schema.InProgress{Migration: Migration0002ID, Step: "promoted", Snapshot: backup}}))
	require.NoError(t, rn.Resume(env))
	log, err := adoption.Read(dir)
	require.NoError(t, err)
	require.Equal(t, 1, log.Count(adoption.RecoveryResumed, time.Time{}))
}

func TestAdoptionLog_MalformedLinesAreCountedNotFatal(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, adoption.Record(dir, adoption.Event{Kind: adoption.RuntimeBoot}))
	f, err := os.OpenFile(adoption.Path(dir), os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, _ = f.WriteString("not json\n{\"at\":\"2026-01-01T00:00:00Z\"}\n")
	require.NoError(t, f.Close())
	log, err := adoption.Read(dir)
	require.NoError(t, err)
	require.Len(t, log.Events, 1)
	require.Equal(t, 2, log.Malformed)

	empty, err := adoption.Read(t.TempDir())
	require.NoError(t, err)
	require.Empty(t, empty.Events)
}
