package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/warden/internal/migrate"
	"github.com/srjn45/warden/internal/ownerlock"
	"github.com/srjn45/warden/internal/schema"
	"github.com/srjn45/warden/internal/updater"
)

func startOwnerHolder(t *testing.T, dir string) *exec.Cmd {
	t.Helper()
	c := exec.Command(os.Args[0])
	c.Env = append(os.Environ(), "WARDEN_CLI_HOLD_DIR="+dir)
	out, err := c.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, c.Start())
	t.Cleanup(func() { _ = c.Process.Kill(); _ = c.Wait() })
	buf := make([]byte, 8)
	n, _ := out.Read(buf)
	require.Equal(t, "OWNER\n", string(buf[:n]))
	return c
}

type idxEntry struct {
	SegmentPath string `json:"segment"`
	Offset      int64  `json:"offset"`
	Rev         uint64 `json:"rev,omitempty"`
}

type idxFile struct {
	Entries  map[string]idxEntry `json:"entries"`
	Checksum string              `json:"checksum"`
}

func writeIdxFile(t *testing.T, path string, entries map[string]idxEntry) {
	t.Helper()
	payload, err := json.Marshal(entries)
	require.NoError(t, err)
	sum := sha256.Sum256(payload)
	f := idxFile{
		Entries:  entries,
		Checksum: hex.EncodeToString(sum[:]),
	}
	out, err := json.Marshal(f)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, out, 0o600))
}

// createStoreWithLiveLag writes a store whose index.json has not caught up
// with the latest appended record in seg_000001.ndjson, yielding CodeIndexStaleTail.
func createStoreWithLiveLag(t *testing.T, storeDir, colName string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(storeDir, 0o700))
	db, err := engine.Open(storeDir, engine.CollectionConfig{})
	require.NoError(t, err)
	col, err := db.Collection(colName)
	require.NoError(t, err)
	_, _, err = col.InsertWithKey("k1", map[string]any{"data": "v1"})
	require.NoError(t, err)
	require.NoError(t, db.Close())

	colDir := filepath.Join(storeDir, colName)

	// Append an update to record 1 directly to seg_000001.ndjson so index trails the segment
	segPath := filepath.Join(colDir, "seg_000001.ndjson")
	line2 := `{"id":1,"op":"update","rev":2,"ts":"2026-10-18T10:01:00Z","data":{"_key":"k1","data":"v2"}}` + "\n"
	f, err := os.OpenFile(segPath, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = f.WriteString(line2)
	require.NoError(t, err)
	require.NoError(t, f.Close())
}

// createStoreWithDanglingIndex creates genuine index corruption (CodeIndexDanglingOffset).
func createStoreWithDanglingIndex(t *testing.T, colDir string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(colDir, 0o700))
	line1 := `{"id":1,"op":"insert","rev":1,"ts":"2026-10-18T10:00:00Z","data":{"_key":"k1"}}` + "\n"
	segPath := filepath.Join(colDir, "seg_000001.ndjson")
	require.NoError(t, os.WriteFile(segPath, []byte(line1), 0o600))

	// index.json with a dangling offset beyond the segment end
	entries := map[string]idxEntry{
		"1": {SegmentPath: "seg_000001.ndjson", Offset: 0, Rev: 1},
		"2": {SegmentPath: "seg_000001.ndjson", Offset: int64(len(line1) + 500), Rev: 1},
	}
	writeIdxFile(t, filepath.Join(colDir, "index.json"), entries)
}

// 1. Owner Present: running daemon's expected ownership lock and only expected
// live index lag do not block target preflight.
func TestUpdaterTargetPreflight_OwnerPresent_LiveIndexLagDoesNotBlock(t *testing.T) {
	d := legacyDataDir(t)
	// Add expected live index lag to context store
	createStoreWithLiveLag(t, filepath.Join(d, "context"), "context")

	// Verify that engine reports CodeIndexStaleTail on the store
	rep, err := engine.VerifyDir(context.Background(), filepath.Join(d, "context"), engine.VerifyOptions{Mode: engine.VerifyFull})
	require.NoError(t, err)
	var foundStaleTail bool
	for _, cr := range rep.Collections {
		for _, f := range cr.Findings {
			if f.Code == engine.CodeIndexStaleTail {
				foundStaleTail = true
			}
		}
	}
	require.True(t, foundStaleTail, "fixture must produce CodeIndexStaleTail")

	// Start live daemon owner
	_ = startOwnerHolder(t, d)

	// Preflight should recognize the live owner and expected index lag as notes
	findings, err := migrate.NewRunner(nil).Check(migrate.Env{DataDir: d, Context: context.Background()}, schema.Schema2)
	require.NoError(t, err)

	res := findingsToPreflightResult(findings)
	require.Empty(t, res.Blockers, "expected owner and expected live index lag must not block: %+v", res.Blockers)
	require.NotEmpty(t, res.Notes, "must report auto-recoverable notes")

	// Notes must mention expected running daemon
	var foundOwnerNote bool
	for _, n := range res.Notes {
		if strings.Contains(n, "a running daemon is expected here") {
			foundOwnerNote = true
		}
	}
	require.True(t, foundOwnerNote, "notes should explain the running daemon is expected")
}

// 2. Owner Absent: stale index lag blocks when daemon is not running.
func TestUpdaterTargetPreflight_OwnerAbsent_IndexLagBlocks(t *testing.T) {
	d := legacyDataDir(t)
	// Add index lag to context store without a live daemon running
	createStoreWithLiveLag(t, filepath.Join(d, "context"), "context")

	// Without an owner, index lag is stale derived state that blocks cutover
	findings, err := migrate.NewRunner(nil).Check(migrate.Env{DataDir: d, Context: context.Background()}, schema.Schema2)
	require.NoError(t, err)

	res := findingsToPreflightResult(findings)
	require.NotEmpty(t, res.Blockers, "stale derived index without a live owner must block preflight")

	var foundIndexBlocker bool
	for _, b := range res.Blockers {
		if strings.Contains(b, "context") && (strings.Contains(b, "index") || strings.Contains(b, "derived")) {
			foundIndexBlocker = true
		}
	}
	require.True(t, foundIndexBlocker, "blocker must identify the stale derived index: %+v", res.Blockers)
}

// 3. Race / New Owner:
// 3a. Race during preflight: a new owner appears during verification.
func TestUpdaterTargetPreflight_RaceDuringPreflight_Blocks(t *testing.T) {
	// Simulate preflight finding where a new owner took the data dir during verification
	findings := []migrate.Finding{
		{
			Severity: migrate.SeverityRepairable,
			Class:    migrate.ClassSchema2CutoverPending,
			Store:    "warden-db",
			Message:  "cutover pending",
			Command:  "warden daemon stop && warden migrate --apply",
		},
		{
			Severity: migrate.SeverityBlocking,
			Class:    string(migrate.ClassOwned),
			Store:    ownerlock.FileName,
			Message:  "a running warden process took the data dir during verification",
			Details:  "wait and retry",
		},
	}

	res := findingsToPreflightResult(findings)
	require.NotEmpty(t, res.Blockers, "a new owner taking data dir during verification must block")
	require.Contains(t, res.Blockers[0], "took the data dir during verification")
}

// 3b. Race post-stop during migrate --apply: a foreign owner holds the lock.
func TestUpdaterPostStop_RaceNewOwner_RefusesWithoutMutation(t *testing.T) {
	d := legacyDataDir(t)

	// Simulate foreign owner acquiring data dir after updater stopped daemon
	_ = startOwnerHolder(t, d)

	// Target migration runner must refuse while owned
	rn := migrate.NewRunner(nil)
	err := rn.Apply(migrate.Env{DataDir: d, BinaryVersion: "11.0.0", Context: context.Background()}, schema.Schema2)
	require.Error(t, err)
	require.ErrorIs(t, err, migrate.ErrRefusedBeforeChange)

	// Verify data directory remains at schema 1 and unmigrated
	l, lerr := schema.Load(d)
	require.NoError(t, lerr)
	require.Equal(t, 1, l.SchemaVersion)
	require.NoDirExists(t, filepath.Join(d, "warden-db"))
}

// 4. Corruption: genuine corruption in savings/ledger.jsonl blocks and
// diagnostic points directly to savings/ledger.jsonl, not to an unrelated backend repair command.
func TestUpdaterTargetPreflight_Corruption_PointsToSavingsLedgerNotBackend(t *testing.T) {
	d := legacyDataDir(t)

	// Write savings/ledger.jsonl with line exceeding limit (cannot determine line boundaries)
	require.NoError(t, os.MkdirAll(filepath.Join(d, "savings"), 0o700))
	// Huge line exceeding 16MiB
	corruptPath := filepath.Join(d, "savings", "ledger.jsonl")
	f, err := os.OpenFile(corruptPath, os.O_CREATE|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	buf := make([]byte, 1024*1024) // 1MB chunk
	for i := range buf {
		buf[i] = 'a'
	}
	for i := 0; i < 17; i++ { // > 16MiB without newline
		_, err := f.Write(buf)
		require.NoError(t, err)
	}
	require.NoError(t, f.Close())

	// Run preflight
	findings, err := migrate.NewRunner(nil).Check(migrate.Env{DataDir: d, Context: context.Background()}, schema.Schema2)
	require.NoError(t, err)

	res := findingsToPreflightResult(findings)
	require.NotEmpty(t, res.Blockers, "corrupted savings/ledger.jsonl must block preflight")

	var foundSavingsBlocker bool
	for _, b := range res.Blockers {
		if strings.Contains(b, "savings/ledger.jsonl") {
			foundSavingsBlocker = true
			require.Contains(t, b, "16MiB limit")
		}
	}
	require.True(t, foundSavingsBlocker, "blockers must point to savings/ledger.jsonl: %+v", res.Blockers)
	require.Empty(t, res.RepairCommand, "must not suggest repair commands for corrupt JSONL")

	// Preflight error returned by updater
	pe := &updater.PreflightError{Blockers: res.Blockers, RepairCommand: res.RepairCommand}
	guidanceErr := updateRecoveryGuidance(pe)
	require.Error(t, guidanceErr)

	errMsg := guidanceErr.Error()
	require.Contains(t, errMsg, "savings/ledger.jsonl", "diagnostic must point to the real savings blocker")
	require.NotContains(t, errMsg, "repair backends", "diagnostic must NOT point to unrelated backend repair command")
	require.NotContains(t, errMsg, "backend registry recovery", "diagnostic must NOT mention backend registry recovery")
	require.NotContains(t, errMsg, "live-wins", "diagnostic must NOT suggest live-wins for malformed JSONL")
}

// 4b. Corruption: unexpected index damage (e.g. dangling offset) remains blocking
// even if a daemon is running.
func TestUpdaterTargetPreflight_UnexpectedIndexDamage_RemainsBlockingWhileOwned(t *testing.T) {
	d := legacyDataDir(t)
	// Add dangling index damage
	createStoreWithDanglingIndex(t, filepath.Join(d, "context", "context"))

	// Start live daemon owner
	_ = startOwnerHolder(t, d)

	// Preflight must NOT treat dangling offset as expected live lag; it must remain blocking
	findings, err := migrate.NewRunner(nil).Check(migrate.Env{DataDir: d, Context: context.Background()}, schema.Schema2)
	require.NoError(t, err)

	res := findingsToPreflightResult(findings)
	require.NotEmpty(t, res.Blockers, "genuine index damage must remain blocking even when owned")

	var foundContextBlocker bool
	for _, b := range res.Blockers {
		if strings.Contains(b, "context") {
			foundContextBlocker = true
		}
	}
	require.True(t, foundContextBlocker, "dangling offset must be in blockers: %+v", res.Blockers)
}

// 4c. Malformed JSON records with valid line boundaries in savings/ledger.jsonl
// are quarantined during cutover and do NOT block preflight.
func TestUpdaterTargetPreflight_MalformedSavingsLines_QuarantinedAndDoNotBlock(t *testing.T) {
	d := legacyDataDir(t)

	// Write savings/ledger.jsonl with malformed record in middle
	line1 := `{"ts":"2026-10-18T10:00:00Z","raw_tokens":10,"saved":5}` + "\n"
	line2 := `{"corrupted": not valid json` + "\n"
	line3 := `{"ts":"2026-10-18T10:02:00Z","raw_tokens":20,"saved":10}` + "\n"
	require.NoError(t, os.MkdirAll(filepath.Join(d, "savings"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(d, "savings", "ledger.jsonl"), []byte(line1+line2+line3), 0o600))

	findings, err := migrate.NewRunner(nil).Check(migrate.Env{DataDir: d, Context: context.Background()}, schema.Schema2)
	require.NoError(t, err)

	res := findingsToPreflightResult(findings)
	require.Empty(t, res.Blockers, "quarantineable savings records must not block: %+v", res.Blockers)

	var foundQuarantineNote bool
	for _, n := range res.Notes {
		if strings.Contains(n, "savings/ledger.jsonl") && strings.Contains(n, "quarantined during cutover") {
			foundQuarantineNote = true
		}
	}
	require.True(t, foundQuarantineNote, "must report quarantine note: %+v", res.Notes)
}
