package backendstore

// Executable contract for issue #841; see
// docs/specs/2026-10-09-backend-registry-integrity-contract.md. Tests named
// TestContract* that are skipped describe behavior t2-t5 must deliver; removing
// the Skip is the acceptance gate for that task.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/srjn45/scriva"
	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/scriva/query"
	"github.com/stretchr/testify/require"
)

// TestContractScrivaDependencyPin forces a re-audit of the contract doc (§3)
// whenever scriva is bumped.
func TestContractScrivaDependencyPin(t *testing.T) {
	gomod, err := os.ReadFile(filepath.Join("..", "..", "go.mod"))
	require.NoError(t, err)
	m := regexp.MustCompile(`github.com/srjn45/scriva (v\S+)`).FindSubmatch(gomod)
	require.NotNil(t, m)
	require.Equal(t, "v1.4.0", string(m[1]),
		"scriva bumped: re-audit docs/specs/2026-10-09-backend-registry-integrity-contract.md §3 and update this pin")
}

// TestContractPreservationHelpersRoundTrip proves the helpers themselves: a
// clean close/reopen preserves every collection, and the snapshot is rich
// enough to notice a lost override.
func TestContractPreservationHelpersRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := buildRegistry(t, dir)
	require.Equal(t, "claude", want.Default)
	require.Equal(t, TierSubscription, backendByID(t, want, "claude").Tier)
	require.False(t, backendByID(t, want, fxCustomBackend).Enabled)
	require.Equal(t, ThinkingModeLocalOnly, want.Settings.InternalThinkingMode)
	require.True(t, want.Settings.AllowPaidAutopilot)
	require.Equal(t, 77, want.Handover.ContextFillThreshold)
	require.Equal(t, []string{fxQuotaBackend + "/" + fxCooldownModel}, want.CoolingFor)
	require.NotEmpty(t, want.Quotas)
	require.NotEmpty(t, want.Models)
	require.NotEmpty(t, want.RoleTiers)

	s, err := NewStore(dir)
	require.NoError(t, err)
	defer s.Close()
	requireRegistryPreserved(t, want, snapshotRegistry(t, s))

	// Negative: a lost tier override must fail the assertion.
	lost := snapshotRegistry(t, s)
	lost.Backends[0].Tier = TierUnclassified
	require.NotEqual(t, want.Backends, lost.Backends)
}

func backendByID(t *testing.T, snap registrySnapshot, id string) Backend {
	t.Helper()
	for _, b := range snap.Backends {
		if b.ID == id {
			return b
		}
	}
	t.Fatalf("backend %q missing from snapshot", id)
	return Backend{}
}

// TestContractRevisionRegressionRefusesOpen reproduces #841: strict open (the
// NewStore default) refuses a registry with revision regressions, naming the
// finding code, and does not rewrite anything.
func TestContractRevisionRegressionRefusesOpen(t *testing.T) {
	dir, _ := damagedRegistry(t, "backends")
	before := readSegment(t, dir, "backends")

	_, err := NewStore(dir)
	require.ErrorIs(t, err, engine.ErrIntegrity)
	var oie *engine.OpenIntegrityError
	require.True(t, errors.As(err, &oie))
	require.Equal(t, "backends", oie.Collection)
	require.Contains(t, err.Error(), "conflict-revision-regression")

	require.Equal(t, before, readSegment(t, dir, "backends"), "a refused open must not mutate segments")
}

// TestContractVerifyClassifiesRegressionAsConflict pins that Verify reports the
// finding as a Conflict (not data corruption) in only the damaged collection.
func TestContractVerifyClassifiesRegressionAsConflict(t *testing.T) {
	dir, _ := damagedRegistry(t, "backends", "role_tiers")
	rep, err := engine.VerifyDir(context.Background(), dir, engine.VerifyOptions{Mode: engine.VerifyFull})
	require.NoError(t, err)
	require.True(t, rep.Has(engine.CodeConflictRevision))
	require.Equal(t, engine.SeverityConflict, rep.MaxSeverity())
	damaged := map[string]bool{}
	for _, c := range rep.Collections {
		if hasFindings(c) {
			damaged[c.Name] = true
		}
	}
	require.Equal(t, map[string]bool{"backends": true, "role_tiers": true}, damaged)
}

// TestContractScrivaRepairDoesNotResolveRegression pins the boundary the
// recovery design rests on: engine.Repair never resolves a revision conflict,
// it only reports it, so t2 cannot rely on Repair alone.
func TestContractScrivaRepairDoesNotResolveRegression(t *testing.T) {
	dir, _ := damagedRegistry(t, "backends")
	rep, err := engine.Repair(context.Background(), dir, engine.RepairOptions{
		Collections: []string{"backends"},
		BackupDir:   t.TempDir(),
		OnConflict:  engine.ConflictReport,
	})
	require.NoError(t, err)
	var cr *engine.CollectionRepair
	for i := range rep.Collections {
		if rep.Collections[i].Name == "backends" {
			cr = &rep.Collections[i]
		}
	}
	require.NotNil(t, cr)
	require.NotEmpty(t, cr.Conflicts, "regressions must be listed, never silently resolved")

	// ConflictAbort stops before any mutation, including the backup.
	dir2, _ := damagedRegistry(t, "backends")
	_, err = engine.Repair(context.Background(), dir2, engine.RepairOptions{
		Collections: []string{"backends"}, BackupDir: t.TempDir(), OnConflict: engine.ConflictAbort,
	})
	require.ErrorIs(t, err, engine.ErrRepairConflict)

	// Observed (scriva v1.4.0): the conflicting history stays on disk (Verify
	// still reports it) but the repair persists a rebuilt index, so a later
	// strict open no longer scans segments and succeeds -- serving the
	// last-write-wins (stale) values. A report-mode Repair therefore HIDES the
	// regression rather than fixing it; t2 must not treat it as recovery.
	rep2, err := engine.VerifyDir(context.Background(), dir, engine.VerifyOptions{Mode: engine.VerifyFull})
	require.NoError(t, err)
	require.True(t, rep2.Has(engine.CodeConflictRevision), "conflict remains after report-mode repair")
	s, err := NewStore(dir)
	require.NoError(t, err)
	require.NoError(t, s.Close())
}

// TestContractReportPolicyIsLastWriteWins documents what salvage-open (the
// policy t2 may use) actually yields: the stale replayed update WINS, so a tier
// the user changed later is silently reverted. A recovery that merely opens
// with PolicyReport is therefore NOT preservation.
func TestContractReportPolicyIsLastWriteWins(t *testing.T) {
	dir, want := damagedRegistry(t, "backends")
	var outcome string
	db, err := scriva.Open(dir, scriva.WithIntegrityPolicy(engine.PolicyReport),
		scriva.WithOnIntegrity(func(c string, _ engine.IntegrityPolicy, o string, _ *engine.CollectionReport) {
			if c == "backends" {
				outcome = o
			}
		}))
	require.NoError(t, err)
	col, err := db.Collection("backends")
	require.NoError(t, err)
	require.Equal(t, engine.IntegrityOutcomeReported, outcome)
	// Observed: under a tolerant open the secondary _key index is not rebuilt,
	// so GetByKey misses (engine.ErrKeyNotFound) while Scan still returns the
	// rows. Store.get/Upsert -- all keyed -- would misbehave on such a handle;
	// t2 must not run the normal Store over a PolicyReport open.
	_, kerr := col.GetByKey("claude")
	require.ErrorIs(t, kerr, engine.ErrKeyNotFound)
	rows, err := col.Scan(query.MatchAll)
	require.NoError(t, err)
	var stale any
	for _, r := range rows {
		if r.Data["id"] == "claude" {
			stale = r.Data["tier"]
		}
	}
	require.NoError(t, db.Close())

	require.NotEqual(t, backendByID(t, want, "claude").Tier, stale,
		"fixture must make the regression observable: stale update wins under PolicyReport")
}

// TestContractUntouchedCollectionsStillOpenClean pins that damage is scoped per
// collection: only the regressed collection carries findings.
func TestContractUntouchedCollectionsStillOpenClean(t *testing.T) {
	dir, _ := damagedRegistry(t, "backends")
	rep, err := engine.VerifyDir(context.Background(), dir, engine.VerifyOptions{Mode: engine.VerifyFull})
	require.NoError(t, err)
	for _, c := range rep.Collections {
		if c.Name != "backends" {
			require.False(t, hasFindings(c), "collection %s", c.Name)
		}
	}
}

// --- pending: behavior t2-t5 must deliver ---------------------------------

// TestContractRecoveryPreservesRegistry: offline, backup-first recovery of a
// regressed registry keeps every user-owned fact as of the LATEST writer
// (contract §5). TODO(t2).
func TestContractRecoveryPreservesRegistry(t *testing.T) {
	t.Skip("pending: TODO(t2) recovery implementation (#841)")
	dir, want := damagedRegistry(t, "backends")
	// t2: run recovery(dir, backupDir) then:
	s, err := NewStore(dir)
	require.NoError(t, err)
	defer s.Close()
	requireRegistryPreserved(t, want, snapshotRegistry(t, s))
}

// TestContractDaemonStartDoesNotRefuseOnDerivedStore: the daemon boots when the
// backends registry has integrity findings (contract §6). TODO(t3).
func TestContractDaemonStartDoesNotRefuseOnDerivedStore(t *testing.T) {
	t.Skip("pending: TODO(t3) daemon/CLI open policy (#841)")
}

// TestContractUpdateSurfacesIntegrityFindings: `wd update` reports findings and
// the repair command before restarting (contract §7). TODO(t4).
func TestContractUpdateSurfacesIntegrityFindings(t *testing.T) {
	t.Skip("pending: TODO(t4) wd update preflight (#841)")
}

func readSegment(t *testing.T, dir, col string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, col, "seg_000001.ndjson"))
	require.NoError(t, err)
	return b
}

// hasFindings reports whether a collection report has anything above info.
func hasFindings(c engine.CollectionReport) bool {
	for _, f := range c.Findings {
		if f.Severity != engine.SeverityInfo {
			return true
		}
	}
	return false
}
