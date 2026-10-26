package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/adoption"
	"github.com/srjn45/warden/internal/audit"
	"github.com/srjn45/warden/internal/migrate"
	"github.com/srjn45/warden/internal/schema"
)

// TestLegacyStoreRetirement_FullLifecycle exercises the complete lifecycle of
// legacy-store retirement under Plan 07 using real warden processes:
//  1. Pre-cutover: the schema-2 binary refuses to run on schema 1, and legacy
//     inspection / cleanup tools refuse because legacy stores are the live authority.
//  2. Upgrade: warden migrate --plan and --apply cut over to schema 2, producing
//     the single warden-db and an immutable, verified pre-schema2 backup.
//  3. Daemon boot and restarts: the daemon boots and restarts on schema 2,
//     proving sole runtime authority on warden-db, serving active agents, and
//     logging adoption heartbeats without touching legacy stores.
//  4. Repair: warden repair warden-db verifies store integrity offline, while
//     legacy repair commands (repair agents/sessions/backends/all) strictly
//     refuse on schema 2 to prevent dual-authority corruption.
//  5. Backup and restore: the cutover backup is verified, and warden migrate
//     --restore refuses when no migration is in progress.
//  6. Legacy inspection: warden inspect legacy safely inspects retained schema-1
//     sources, diffs keys against warden-db, and verifies the cutover backup.
//  7. Retirement criteria evaluation: warden migrate --retirement evaluates the
//     9 stable entry criteria; before the 90-day window it reports ineligible,
//     and after the window elapses it reports eligible.
//  8. Safe backup-gated cleanup: warden migrate --cleanup-legacy requires
//     confirmation, dry-runs without mutation, quarantines exact legacy paths
//     into backups/legacy-quarantine-*, keeps external artifacts in place, logs
//     audit and adoption events, verifies uninterrupted daemon operation on
//     warden-db, and proves idempotency and recoverability.
func TestLegacyStoreRetirement_FullLifecycle(t *testing.T) {
	e := newEnv(t)
	e.seedLegacy()

	// Seed external transcript under snapshots (a migrated source with SkipSuffix)
	require.NoError(t, os.MkdirAll(filepath.Join(e.data, "snapshots"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(e.data, "snapshots", "snap-1.transcript"), []byte("external transcript content"), 0o600))

	// Start live tmux sessions for seeded agents
	for _, id := range []string{"a-1", "a-2"} {
		require.NoError(t, e.tmux("new-session", "-d", "-s", "tm-"+id, "sleep", "600"))
	}
	cfg := e.config(freeAddr(t), e.data)

	// -------------------------------------------------------------------------
	// 1. Pre-cutover authority guard
	// -------------------------------------------------------------------------
	// Daemon refuses schema-1 data before opening any store.
	d := e.startDaemon(freeAddr(t), e.data, e.root)
	daemonLog := d.waitExit()
	require.Contains(t, daemonLog, "must be cut over to the single warden-db")
	require.False(t, schema.WardenDBPresent(e.data))

	// warden inspect legacy refuses on schema-1 data dir (legacy is live authority)
	out, code := e.run(e.root, "--config", cfg, "inspect", "legacy")
	require.NotZero(t, code, out)
	require.Contains(t, out, "legacy stores are the live authority")

	// warden migrate --cleanup-legacy refuses on schema-1 data dir
	out, code = e.run(e.root, "--config", cfg, "migrate", "--cleanup-legacy", "--yes")
	require.NotZero(t, code, out)

	// -------------------------------------------------------------------------
	// 2. Upgrade from schema 1 to schema 2
	// -------------------------------------------------------------------------
	out, code = e.run(e.root, "--config", cfg, "migrate", "--plan", "--target", "2")
	require.Zero(t, code, out)
	require.Contains(t, out, "ready")

	out, code = e.run(e.root, "--config", cfg, "migrate", "--apply", "--target", "2")
	require.Zero(t, code, out)

	ledger, err := schema.Load(e.data)
	require.NoError(t, err)
	require.Equal(t, schema.Schema2, ledger.SchemaVersion)
	require.Nil(t, ledger.InProgress)
	require.True(t, schema.WardenDBPresent(e.data))

	// Verify immutable backup under backups/pre-schema2-*
	backups, err := filepath.Glob(filepath.Join(e.data, "backups", migrate.Schema2BackupPrefix+"*"))
	require.NoError(t, err)
	require.Len(t, backups, 1)
	backupDir := backups[0]
	_, err = migrate.VerifySchema2Backup(backupDir)
	require.NoError(t, err)

	// -------------------------------------------------------------------------
	// 3. Daemon boot and restarts on schema 2 (sole runtime authority)
	// -------------------------------------------------------------------------
	// Boot the released schema-2 waypoint. The current binary is format 3 and
	// must refuse this historical generation rather than becoming its runtime.
	waypoint := waypointBinary(t)
	d1 := e.startDaemonBin(waypoint, freeAddr(t), e.data, e.root)
	d1.waitUp()
	code, gotFleet := fleet(t, d1)
	require.Equal(t, 200, code)
	require.Equal(t, []string{"tm-a-1", "tm-a-2"}, gotFleet)
	var hz struct {
		Status        string `json:"status"`
		SchemaVersion int    `json:"schema_version"`
	}
	_, body := get(t, d1.base()+"/healthz")
	require.NoError(t, json.Unmarshal(body, &hz))
	require.Equal(t, "ok", hz.Status)
	require.Equal(t, schema.Schema2, hz.SchemaVersion)
	d1.stop()

	// Boot 2 (restart 1)
	d2 := e.startDaemonBin(waypoint, freeAddr(t), e.data, e.root)
	d2.waitUp()
	code, gotFleet = fleet(t, d2)
	require.Equal(t, 200, code)
	require.Equal(t, []string{"tm-a-1", "tm-a-2"}, gotFleet)
	d2.stop()

	// Boot 3 (restart 2): completes minimum required runtime boots (>= 3)
	d3 := e.startDaemonBin(waypoint, freeAddr(t), e.data, e.root)
	d3.waitUp()
	code, gotFleet = fleet(t, d3)
	require.Equal(t, 200, code)
	require.Equal(t, []string{"tm-a-1", "tm-a-2"}, gotFleet)
	d3.stop()

	// Verify adoption evidence logged 3 runtime boots
	adoptLog, err := adoption.Read(e.data)
	require.NoError(t, err)
	require.GreaterOrEqual(t, adoptLog.Count(adoption.RuntimeBoot, time.Time{}), 3)

	// -------------------------------------------------------------------------
	// 4. Offline repair: warden repair warden-db vs refusing legacy dual-authority
	// -------------------------------------------------------------------------
	// Offline repair of warden-db succeeds in dry-run
	out, code = e.run(e.root, "--config", cfg, "repair", "warden-db", "--dry-run")
	require.Zero(t, code, out)
	require.Contains(t, out, "warden-db repair dry-run: health=healthy blocking=0 recoverable=0; no files changed")

	// Legacy repair tools refuse on schema 2 with explicit migration guidance
	for _, sub := range []string{"agents", "sessions", "backends", "all"} {
		out, code = e.run(e.root, "--config", cfg, "repair", sub)
		require.NotZero(t, code, "repair %s must refuse on schema 2: %s", sub, out)
		require.Contains(t, out, "this data directory is on schema 2 (warden-db); the schema-1 store directories are inert copies")
		require.Contains(t, out, "warden repair warden-db")
	}

	// -------------------------------------------------------------------------
	// 5. Backup & restore (warden migrate --restore)
	// -------------------------------------------------------------------------
	// Calling --restore without an interrupted migration refuses
	out, code = e.run(e.root, "--config", cfg, "migrate", "--restore", "--target", "2")
	require.NotZero(t, code, out)
	require.Contains(t, out, "no migration in progress to resume")

	// Cutover backup remains intact and passes verification
	_, err = migrate.VerifySchema2Backup(backupDir)
	require.NoError(t, err)

	// -------------------------------------------------------------------------
	// 6. Legacy inspection (warden inspect legacy)
	// -------------------------------------------------------------------------
	out, code = e.run(e.root, "--config", cfg, "inspect", "legacy", "--json")
	require.Zero(t, code, out)
	var insp migrate.LegacyInspection
	require.NoError(t, json.Unmarshal([]byte(out), &insp), out)
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
			require.Equal(t, 2, col.CanonRecords)
		}
	}
	require.True(t, foundAgentsCol, "agents collection inspected")

	// -------------------------------------------------------------------------
	// 7. Retirement criteria evaluation (warden migrate --retirement)
	// -------------------------------------------------------------------------
	// Since cutover just occurred, support_window_elapsed (90 days) is the sole blocker
	out, code = e.run(e.root, "--config", cfg, "migrate", "--retirement", "--json")
	require.Zero(t, code, out)
	var ret migrate.RetirementReport
	require.NoError(t, json.Unmarshal([]byte(out), &ret), out)
	require.False(t, ret.Eligible, "support window has not elapsed yet")
	require.Equal(t, []string{migrate.CritSupportWindow}, ret.Blockers)
	for _, c := range ret.Criteria {
		if c.ID != migrate.CritSupportWindow {
			require.True(t, c.Met, "criterion %s should be met: %s", c.ID, c.Detail)
		}
	}

	// Backdate cutover timestamp in schema.json ledger to simulate support window passing (>90 days)
	ledger, err = schema.Load(e.data)
	require.NoError(t, err)
	require.NotEmpty(t, ledger.History)
	for i := range ledger.History {
		ledger.History[i].At = time.Now().UTC().Add(-95 * 24 * time.Hour)
	}
	require.NoError(t, schema.Save(e.data, ledger))

	// Re-evaluate retirement: now eligible!
	out, code = e.run(e.root, "--config", cfg, "migrate", "--retirement", "--json")
	require.Zero(t, code, out)
	ret = migrate.RetirementReport{}
	require.NoError(t, json.Unmarshal([]byte(out), &ret), out)
	require.True(t, ret.Eligible, "all criteria met after support window: %+v", ret)
	require.Empty(t, ret.Blockers)

	// -------------------------------------------------------------------------
	// 8. Safe backup-gated cleanup (warden migrate --cleanup-legacy)
	// -------------------------------------------------------------------------
	// 8a. Dry run: lists candidates, does not move any file
	out, code = e.run(e.root, "--config", cfg, "migrate", "--cleanup-legacy", "--dry-run", "--json")
	require.Zero(t, code, out)
	var dryRep migrate.CleanupReport
	require.NoError(t, json.Unmarshal([]byte(out), &dryRep), out)
	require.True(t, dryRep.DryRun)
	require.False(t, dryRep.Applied)
	require.Contains(t, dryRep.Paths, "agents-db")
	require.Contains(t, dryRep.Kept, "snapshots")
	require.DirExists(t, filepath.Join(e.data, "agents-db"))
	require.FileExists(t, filepath.Join(e.data, "snapshots", "snap-1.transcript"))

	// 8b. Refuses unconfirmed without --yes
	out, code = e.run(e.root, "--config", cfg, "migrate", "--cleanup-legacy")
	require.NotZero(t, code, out)
	require.Contains(t, out, "not confirmed")
	require.DirExists(t, filepath.Join(e.data, "agents-db"))

	// 8c. Confirmed cleanup with --yes
	out, code = e.run(e.root, "--config", cfg, "migrate", "--cleanup-legacy", "--yes", "--json")
	require.Zero(t, code, out)
	var cleanRep migrate.CleanupReport
	require.NoError(t, json.Unmarshal([]byte(out), &cleanRep), out)
	require.True(t, cleanRep.Applied)
	require.NotEmpty(t, cleanRep.Quarantine)

	// Verify migrated legacy paths were quarantined (moved, not deleted)
	for _, p := range cleanRep.Paths {
		require.NoFileExists(t, filepath.Join(e.data, filepath.FromSlash(p)), "must be removed from live data dir: %s", p)
		_, err := os.Lstat(filepath.Join(cleanRep.Quarantine, filepath.FromSlash(p)))
		require.NoError(t, err, "must exist in quarantine: %s", p)
	}
	// Kept paths must remain in live data dir
	require.FileExists(t, filepath.Join(e.data, "snapshots", "snap-1.transcript"), "snapshots transcript must be kept in place")
	// warden-db must remain present and intact
	require.DirExists(t, filepath.Join(e.data, "warden-db"))

	// Verify audit log has legacy_cleanup action
	evs, err := audit.Read(filepath.Join(e.data, "audit.jsonl"), audit.Filter{})
	require.NoError(t, err)
	var foundAudit bool
	for _, ev := range evs {
		if ev.Action == audit.ActionLegacyCleanup {
			foundAudit = true
			require.Equal(t, "cli", ev.Actor)
			require.Equal(t, strings.Join(cleanRep.Paths, ","), ev.Detail["paths"])
			relQuar, _ := filepath.Rel(e.data, cleanRep.Quarantine)
			require.Equal(t, filepath.ToSlash(relQuar), ev.Detail["quarantine"])
		}
	}
	require.True(t, foundAudit, "audit log must record legacy_cleanup")

	// Verify adoption evidence has legacy_cleanup event
	adoptLog, err = adoption.Read(e.data)
	require.NoError(t, err)
	require.Equal(t, 1, adoptLog.Count(adoption.LegacyCleanup, time.Time{}))

	// 8d. Idempotent re-run: cleanup is a safe no-op when nothing remains
	out, code = e.run(e.root, "--config", cfg, "migrate", "--cleanup-legacy", "--yes", "--json")
	require.Zero(t, code, out)
	var rerunRep migrate.CleanupReport
	require.NoError(t, json.Unmarshal([]byte(out), &rerunRep), out)
	require.False(t, rerunRep.Applied)
	require.Empty(t, rerunRep.Paths)
	adoptLog, _ = adoption.Read(e.data)
	require.Equal(t, 1, adoptLog.Count(adoption.LegacyCleanup, time.Time{}), "no duplicate adoption event on no-op re-run")

	// 8e. Verified schema-2 operation continues uninterrupted
	dClean := e.startDaemon(freeAddr(t), e.data, e.root)
	dClean.waitUp()
	code, gotFleet = fleet(t, dClean)
	require.Equal(t, 200, code)
	require.Equal(t, []string{"tm-a-1", "tm-a-2"}, gotFleet)
	_, body = get(t, dClean.base()+"/healthz")
	require.NoError(t, json.Unmarshal(body, &hz))
	require.Equal(t, "ok", hz.Status)
	require.Equal(t, schema.Schema2, hz.SchemaVersion)
	dClean.stop()

	// 8f. Recovery test: moving files back from quarantine restores legacy stores
	for _, p := range cleanRep.Paths {
		src := filepath.Join(cleanRep.Quarantine, filepath.FromSlash(p))
		dst := filepath.Join(e.data, filepath.FromSlash(p))
		_ = os.RemoveAll(dst)
		require.NoError(t, os.Rename(src, dst))
	}
	require.DirExists(t, filepath.Join(e.data, "agents-db"))
}
