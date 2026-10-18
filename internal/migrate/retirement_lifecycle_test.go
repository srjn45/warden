package migrate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/adoption"
	"github.com/srjn45/warden/internal/audit"
	"github.com/srjn45/warden/internal/schema"
	"github.com/srjn45/warden/internal/storage"
	"github.com/srjn45/warden/internal/store"
)

// TestRetirementLifecycleRegression verifies the full legacy store retirement
// lifecycle at the migrate package level:
//  1. Cutover from schema 1 to schema 2 creates warden-db and immutable backup.
//  2. Legacy inspection verifies equivalence without mutating data directory.
//  3. Retirement criteria gate enforces the 90-day support window and runtime boots.
//  4. Safe cleanup enforces backup integrity, confirmation, explicit quarantine,
//     audit logging, and idempotency.
//  5. Unified warden-db remains healthy and fully accessible after legacy removal.
//  6. Legacy storage opening is strictly refused (zero runtime fallback).
func TestRetirementLifecycleRegression(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	seedRealisticLegacy(t, dir)

	// Add an external snapshot transcript to verify kept paths
	snapDir := filepath.Join(dir, "snapshots")
	require.NoError(t, os.MkdirAll(snapDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(snapDir, "agent-1.transcript"), []byte("transcript bytes"), 0o600))

	// Pre-cutover: legacy inspection must refuse on schema 1
	_, err := InspectLegacy(ctx, dir)
	require.ErrorIs(t, err, ErrLegacyLiveAuthority)

	// Pre-cutover: cleanup must refuse on schema 1
	_, err = CleanupLegacy(ctx, dir, CleanupOptions{Confirm: true, BinaryVersion: "11.0.0"})
	require.Error(t, err)

	// 1. Cutover from schema 1 to schema 2
	rn := NewRunner(DefaultRegistry)
	env := Env{DataDir: dir, BinaryVersion: "11.0.0", Context: ctx}
	require.NoError(t, rn.Apply(env, schema.Schema2))

	ledger, err := schema.Load(dir)
	require.NoError(t, err)
	require.Equal(t, schema.Schema2, ledger.SchemaVersion)
	require.Nil(t, ledger.InProgress)
	require.True(t, schema.WardenDBPresent(dir))
	require.NotEmpty(t, ledger.History)
	cutoverEntry := ledger.History[len(ledger.History)-1]
	require.Equal(t, Migration0002ID, cutoverEntry.Migration)

	// Verify immutable backup
	backupDir := cutoverEntry.Backup
	if !filepath.IsAbs(backupDir) {
		backupDir = filepath.Join(dir, filepath.FromSlash(backupDir))
	}
	manifest, err := VerifySchema2Backup(backupDir)
	require.NoError(t, err)
	require.NotEmpty(t, manifest.Backup.Files)

	// 2. Legacy inspection
	insp, err := InspectLegacy(ctx, dir)
	require.NoError(t, err)
	require.True(t, insp.ReadOnly)
	require.Equal(t, "none", insp.Authority)
	require.True(t, insp.Unchanged)
	require.True(t, insp.Backup.Verified)
	require.NotEmpty(t, insp.Sources)

	var foundAgentsCol bool
	for _, col := range insp.Collections {
		if col.Name == "agents" {
			foundAgentsCol = true
			require.True(t, col.Match)
			require.Greater(t, col.CanonRecords, 0)
		}
	}
	require.True(t, foundAgentsCol, "agents collection verified in inspection")

	// 3. Retirement evaluation
	// Before simulated boots and before support window
	ret, err := EvaluateRetirement(dir, RetirementOptions{Now: cutoverEntry.At})
	require.NoError(t, err)
	require.False(t, ret.Eligible)
	require.Contains(t, ret.Blockers, CritRuntimeBoots)
	require.Contains(t, ret.Blockers, CritSupportWindow)

	// Record minimum required boots (3 boots)
	recordBoots(t, dir, 3)

	// Still ineligible if support window not elapsed
	ret, err = EvaluateRetirement(dir, RetirementOptions{Now: cutoverEntry.At.Add(24 * time.Hour)})
	require.NoError(t, err)
	require.False(t, ret.Eligible)
	require.Equal(t, []string{CritSupportWindow}, ret.Blockers)

	// After support window (>90 days)
	eligibleTime := cutoverEntry.At.Add(DefaultSupportWindow + time.Hour)
	ret, err = EvaluateRetirement(dir, RetirementOptions{Now: eligibleTime})
	require.NoError(t, err)
	require.True(t, ret.Eligible)
	require.Empty(t, ret.Blockers)

	// 4. Safe backup-gated cleanup
	// Dry run changes nothing
	dryRep, err := CleanupLegacy(ctx, dir, CleanupOptions{
		DryRun:     true,
		Now:        eligibleTime,
		Retirement: RetirementOptions{Now: eligibleTime},
	})
	require.NoError(t, err)
	require.True(t, dryRep.DryRun)
	require.False(t, dryRep.Applied)
	require.Contains(t, dryRep.Paths, "agents-db")
	require.Contains(t, dryRep.Kept, "snapshots")
	require.DirExists(t, filepath.Join(dir, "agents-db"))
	require.FileExists(t, filepath.Join(snapDir, "agent-1.transcript"))

	// Unconfirmed call refuses
	_, err = CleanupLegacy(ctx, dir, CleanupOptions{
		Confirm:    false,
		Now:        eligibleTime,
		Retirement: RetirementOptions{Now: eligibleTime},
	})
	require.ErrorIs(t, err, ErrCleanupUnconfirmed)

	// Confirmed cleanup moves candidate paths into quarantine
	cleanRep, err := CleanupLegacy(ctx, dir, CleanupOptions{
		Confirm:       true,
		Actor:         "test-operator",
		BinaryVersion: "11.0.0",
		Now:           eligibleTime,
		Retirement:    RetirementOptions{Now: eligibleTime},
	})
	require.NoError(t, err)
	require.True(t, cleanRep.Applied)
	require.NotEmpty(t, cleanRep.Quarantine)

	// Verify migrated legacy paths were removed from live data dir and exist in quarantine
	for _, p := range cleanRep.Paths {
		require.NoFileExists(t, filepath.Join(dir, filepath.FromSlash(p)))
		_, err := os.Lstat(filepath.Join(cleanRep.Quarantine, filepath.FromSlash(p)))
		require.NoError(t, err, "must exist in quarantine: %s", p)
	}

	// External/kept paths stay in live data dir
	require.FileExists(t, filepath.Join(snapDir, "agent-1.transcript"))
	// warden-db remains present
	require.DirExists(t, filepath.Join(dir, "warden-db"))

	// Verify audit log
	evs, err := audit.Read(filepath.Join(dir, "audit.jsonl"), audit.Filter{})
	require.NoError(t, err)
	var foundAudit bool
	for _, ev := range evs {
		if ev.Action == audit.ActionLegacyCleanup {
			foundAudit = true
			require.Equal(t, "test-operator", ev.Actor)
			require.Equal(t, strings.Join(cleanRep.Paths, ","), ev.Detail["paths"])
		}
	}
	require.True(t, foundAudit, "audit event must be logged")

	// Verify adoption evidence
	adoptLog, err := adoption.Read(dir)
	require.NoError(t, err)
	require.Equal(t, 1, adoptLog.Count(adoption.LegacyCleanup, time.Time{}))

	// Idempotent re-run is a no-op
	againRep, err := CleanupLegacy(ctx, dir, CleanupOptions{
		Confirm:       true,
		Actor:         "test-operator",
		BinaryVersion: "11.0.0",
		Now:           eligibleTime.Add(time.Second),
		Retirement:    RetirementOptions{Now: eligibleTime.Add(time.Second)},
	})
	require.NoError(t, err)
	require.False(t, againRep.Applied)
	require.Empty(t, againRep.Paths)
	adoptLog, _ = adoption.Read(dir)
	require.Equal(t, 1, adoptLog.Count(adoption.LegacyCleanup, time.Time{}), "no duplicate adoption event on no-op re-run")

	// 5. Unified storage operations continue uninterrupted on warden-db
	s, err := storage.Open(dir)
	require.NoError(t, err)
	defer s.Close()
	require.NotNil(t, s.DB)

	// Live agents readable from warden-db
	liveAgent, err := s.Agents.Get(ctx, "agent-live")
	require.NoError(t, err)
	require.Equal(t, store.StatusWorking, liveAgent.Status)

	// Active autopilot run readable
	run, err := s.AutopilotRuns.Get(rfRun)
	require.NoError(t, err)
	require.Equal(t, rfPlan, run.PlanID)

	// 6. Legacy storage opening is refused (legacy runtime fallback retired)
	_, err = storage.OpenLegacy(dir)
	require.Error(t, err)
	var guardErr *schema.GuardError
	require.ErrorAs(t, err, &guardErr)
	require.Equal(t, schema.VerdictDataNewer, guardErr.Verdict)

	// OpenLegacy refusal is recorded in adoption evidence
	adoptLog, err = adoption.Read(dir)
	require.NoError(t, err)
	require.Equal(t, 1, adoptLog.Count(adoption.LegacyOpenRefused, time.Time{}))
}
