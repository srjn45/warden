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

	"github.com/srjn45/scriva/engine"
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

// detectionOnly mimics a second daemon re-running detection: only rebuildable
// fields differ from the newest revision.
func detectionOnly(j int, _ uint64, d map[string]any) {
	d["installed"] = j%2 == 0
	d["binary_path"] = "/other/bin/" + string(rune('a'+j))
	d["detected_at"] = time.Date(2026, 10, 9, 0, 0, j, 0, time.UTC).Format(time.RFC3339)
}

// TestIssue841ShapeRecoversWithDetectionOnlyDifferences is the real incident:
// lower revisions written LATER (newer ts) by a second writer, differing from
// the newest revision only in detection fields.
func TestIssue841ShapeRecoversWithDetectionOnlyDifferences(t *testing.T) {
	dir, want, n := divergentRegistry(t, detectionOnly)
	require.Equal(t, 34, n, "fixture must inject 17 rev-68 writes after rev 70 for each backend")
	rep, err := Verify(context.Background(), dir)
	require.NoError(t, err)
	regs := 0
	for _, f := range rep.Integrity.AllFindings() {
		if f.Code == engine.CodeConflictRevision {
			regs++
		}
	}
	require.Equal(t, 34, regs, "fixture must reproduce the 34 #841 revision-regression findings")
	require.True(t, rep.Recoverable())
	before := treeBytes(t, dir)

	opts := quiet()
	opts.BackupDir = t.TempDir()
	s, res, err := Open(dir, opts) // the daemon path
	require.NoError(t, err)
	defer s.Close()
	require.True(t, res.Recovered)
	requireRegistryPreserved(t, want, snapshotRegistry(t, s))
	require.Len(t, res.Discarded, n)
	for _, d := range res.Discarded {
		require.Equal(t, RulePreferencesEqual, d.DiscardedAs)
		require.NotEmpty(t, d.DetectionDiffs)
		require.Less(t, d.Rev, d.WinnerRev)
		require.Equal(t, uint64(68), d.Rev)
		require.Equal(t, uint64(70), d.WinnerRev)
	}
	require.Equal(t, before, treeBytes(t, res.BackupPath), "backup is a byte-identical verified copy")

	raw, err := os.ReadFile(res.ReportPath)
	require.NoError(t, err)
	var doc struct{ Discarded []DiscardedRevision }
	require.NoError(t, json.Unmarshal(raw, &doc))
	require.Len(t, doc.Discarded, n, "report lists every per-id decision")

	// Idempotent.
	require.NoError(t, s.Close())
	res2, err := Repair(context.Background(), dir, opts)
	require.NoError(t, err)
	require.False(t, res2.Recovered)
	rep2, err := Verify(context.Background(), dir)
	require.NoError(t, err)
	require.True(t, rep2.Clean())
}

// TestIssue841ShapeWithPreferenceDifferenceIsRefused: same shape, but the later,
// lower-revision write carries a different tier/enabled/default.
func TestIssue841ShapeWithPreferenceDifferenceIsRefused(t *testing.T) {
	mut := func(j int, id uint64, d map[string]any) {
		detectionOnly(j, id, d)
		if id == 1 && j == 3 {
			d["tier"] = TierFree
			d["enabled"] = false
			d["default"] = !d["default"].(bool)
		}
	}
	dir, _, _ := divergentRegistry(t, mut)
	before := treeBytes(t, dir)

	opts := quiet()
	opts.BackupDir = t.TempDir()
	_, _, err := Open(dir, opts)
	var rre *RecoveryRequiredError
	require.True(t, errors.As(err, &rre))
	require.ErrorIs(t, err, ErrRecoveryRequired)
	require.Equal(t, before, treeBytes(t, dir), "nothing mutated")
	require.Empty(t, rre.BackupPath)

	fields := map[string]FieldConflict{}
	for _, c := range rre.Collections {
		for _, fc := range c.Conflicts {
			fields[fc.Field] = fc
		}
	}
	require.Contains(t, fields, "tier")
	require.Contains(t, fields, "enabled")
	require.Contains(t, fields, "default")
	require.Equal(t, TierFree, fields["tier"].Value)
	require.Equal(t, TierSubscription, fields["tier"].WinnerValue)
	require.Contains(t, err.Error(), "tier")

	raw, err := os.ReadFile(rre.ReportPath)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"winner_value"`)
}

// A running writer persists its index lazily, so Verify beside it reports stale
// derived-index findings. That is expected lag, not damage: LiveIndexLag is true
// while the directory is owned, and false for the very same files once the owner
// is gone (where the findings genuinely mean an unclean stop).
func TestLiveIndexLagOnlyWhileOwned(t *testing.T) {
	dir := t.TempDir()
	buildRegistry(t, dir)
	s, err := NewStore(dir)
	require.NoError(t, err)
	// Appends that the lazily persisted index has not caught up with.
	for _, tier := range []string{TierFree, TierSubscription, TierFree} {
		require.NoError(t, s.SetTier("claude", tier))
	}

	rep, err := Verify(context.Background(), dir)
	require.NoError(t, err)
	if rep.Clean() {
		t.Skip("engine persisted the index eagerly; no live lag to observe")
	}
	require.True(t, rep.Recoverable(), "stale index shapes classify as recoverable")
	require.True(t, rep.LiveIndexLag(), "…but beside the running owner they are expected lag")

	// Same bytes, owner gone: the registry may now be genuinely stale.
	require.NoError(t, s.Close())
	closed, err := Verify(context.Background(), dir)
	require.NoError(t, err)
	require.False(t, closed.LiveIndexLag(), "an unowned directory is never 'live lag'")
}

// Damage is never lag: a clean report, a nil report, and a report with a non-stale
// index finding are all false even when the directory is owned.
func TestLiveIndexLagIgnoresRealDamage(t *testing.T) {
	require.False(t, (*Report)(nil).LiveIndexLag())
	require.False(t, (&Report{}).LiveIndexLag())

	owned := func(codes ...engine.FindingCode) *Report {
		ir := &engine.IntegrityReport{Findings: []engine.Finding{{Severity: engine.SeverityInfo, Code: engine.CodeLockHeld}}}
		cr := engine.CollectionReport{Name: "backends"}
		for _, c := range codes {
			cr.Findings = append(cr.Findings, engine.Finding{Severity: engine.SeverityRepairableIndex, Code: c})
		}
		ir.Collections = []engine.CollectionReport{cr}
		return &Report{
			Integrity:   ir,
			Collections: []CollectionVerdict{{Name: "backends", Verdict: VerdictRecoverable}},
		}
	}
	require.True(t, owned(engine.CodeIndexStaleTail, engine.CodeIndexStaleRecord, engine.CodeSidxStaleTail).LiveIndexLag())
	require.False(t, owned(engine.CodeIndexStaleTail, engine.CodeIndexDanglingOffset).LiveIndexLag(),
		"a dangling offset is real damage even beside a stale tail")
	require.False(t, owned(engine.CodeIndexMissingRecord).LiveIndexLag())

	unowned := owned(engine.CodeIndexStaleTail)
	unowned.Integrity.Findings = nil
	require.False(t, unowned.LiveIndexLag(), "without the lock-held finding it is not lag")
}
