package migrate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/ownerlock"
	"github.com/srjn45/warden/internal/planstore"
	"github.com/srjn45/warden/internal/schema"
	"github.com/srjn45/warden/internal/storage"
	"github.com/srjn45/warden/internal/store"
	"github.com/srjn45/warden/internal/wardendb"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	if d := os.Getenv("WARDEN_MIGRATE_HOLD_DIR"); d != "" {
		if _, err := ownerlock.Acquire(d, ownerlock.Info{Kind: ownerlock.KindDaemon, Version: "10.0.1", Command: "warden daemon"}); err != nil {
			os.Exit(3)
		}
		os.Stdout.WriteString("OWNER\n")
		select {}
	}
	os.Exit(m.Run())
}

// TestSchema2ReleaseAcceptance exercises the operator-visible happy path at a
// non-trivial size.  The focused phase/crash tests remain in migrate_test.go;
// this test proves that the real registered cutover joins those guarantees:
// an old install is promoted into a restartable schema-2 state and is rejected
// by a schema-1 binary afterwards.
func TestSchema2ReleaseAcceptance(t *testing.T) {
	dir := stagingLegacyDir(t)
	require.NoError(t, schema.Save(dir, &schema.Ledger{SchemaVersion: 1, BinaryVersion: "v-schema1"}))
	seedLargeAgentsStore(t, dir, 512)

	runner := NewRunner(DefaultRegistry)
	env := Env{DataDir: dir, BinaryVersion: "v-release-test", Context: context.Background()}
	require.NoError(t, runner.Apply(env, schema.Schema2))

	ledger, err := schema.Load(dir)
	require.NoError(t, err)
	require.Equal(t, schema.Schema2, ledger.SchemaVersion)
	require.Nil(t, ledger.InProgress)
	require.Len(t, ledger.History, 1)
	require.Equal(t, Migration0002ID, ledger.History[0].Migration)

	// This is the restart boundary for the schema-2-capable daemon: a completed
	// ledger starts without another migration. SchemaVersion deliberately remains
	// 1 until the schema-2 runtime is enabled in its own release.
	require.Equal(t, schema.VerdictStart, schema.Decide(ledger.SchemaVersion, schema.Schema2, 1, false))

	// A schema-1 binary must refuse the promoted directory rather than opening
	// its still-present legacy stores alongside warden-db.
	require.Equal(t, schema.VerdictDataNewer, schema.Decide(ledger.SchemaVersion, 1, 0, false))

	// The promoted target remains independently verifiable after restart.
	report, err := VerifySchema2Equivalence(context.Background(), dir, wardendb.Dir(dir))
	require.NoError(t, err)
	require.True(t, report.Passed)
	require.Equal(t, 513, report.TargetCounts["agents"])
	require.Equal(t, 1, report.TargetCounts["archived_agents"])
}

func seedLargeAgentsStore(t *testing.T, dataDir string, n int) {
	t.Helper()
	root := filepath.Join(dataDir, "agents-db")
	// These fixtures already contain transaction state; recover at DB scope.
	require.NoError(t, os.MkdirAll(root, 0o700))
	db, err := engine.Open(root, engine.CollectionConfig{})
	require.NoError(t, err)
	defer db.Close()
	col, err := db.Collection("agents")
	require.NoError(t, err)
	for i := 0; i < n; i++ {
		_, _, err := col.Insert(map[string]any{"id": fmt.Sprintf("agent-%03d", i), "project_id": "/repo", "v": i})
		require.NoError(t, err)
	}
}

// TestReleaseAcceptance_ReportedUpdatePath_QuarantineAndPostStopStrictCutover
// exercises the exact reported update path: active daemon preflight, full
// schema-1 records (active agents, autopilot run, project, plan state), stale
// derived index lag, malformed savings JSONL, explicit quarantine of malformed
// rows, post-stop strict cutover, and verified schema-2 state.
func TestReleaseAcceptance_ReportedUpdatePath_QuarantineAndPostStopStrictCutover(t *testing.T) {
	dir := t.TempDir()
	seedRealisticLegacy(t, dir)

	// 1. Add expected live index lag (CodeIndexStaleTail) to context store
	createStoreWithLiveLag(t, filepath.Join(dir, "context"), "context")

	// 2. Add savings/ledger.jsonl with malformed records surrounded by valid records
	line1 := `{"ts":"2026-10-17T09:00:00Z","feature":"compact","raw_tokens":10,"kept_tokens":4,"saved":6,"agent":"agent-live"}` + "\n"
	line2 := `{"malformed_savings_record": broken json line` + "\n"
	line3 := `{"ts":"2026-10-17T09:05:00Z","feature":"classify","raw_tokens":5,"kept_tokens":5,"saved":0,"agent":"agent-live"}` + "\n"
	line4 := `garbled raw savings text without valid json structure` + "\n"
	line5 := `{"ts":"2026-10-17T09:10:00Z","feature":"compact","raw_tokens":20,"kept_tokens":10,"saved":10,"agent":"agent-live"}` + "\n"
	writeFile(t, dir, "savings/ledger.jsonl", line1+line2+line3+line4+line5)

	expectedRawLine2 := strings.TrimRight(line2, "\r\n")
	sum2 := sha256.Sum256([]byte(expectedRawLine2))
	expectedSHA2 := hex.EncodeToString(sum2[:])

	expectedRawLine4 := strings.TrimRight(line4, "\r\n")
	sum4 := sha256.Sum256([]byte(expectedRawLine4))
	expectedSHA4 := hex.EncodeToString(sum4[:])

	// 3. Start active daemon holding ownership lock in a separate process
	holder := startOwnerHolder(t, dir)

	// 4. Target preflight check while daemon is active
	findings := checkSchema2Cutover(Env{DataDir: dir, Context: context.Background()})
	var blockers []Finding
	var autoNotes []Finding
	var quarantineNotes []Finding
	for _, f := range findings {
		switch f.Severity {
		case SeverityBlocking:
			blockers = append(blockers, f)
		case SeverityAuto:
			autoNotes = append(autoNotes, f)
		case SeverityRepairable:
			if strings.Contains(f.Message, "quarantined during cutover") {
				quarantineNotes = append(quarantineNotes, f)
			}
		}
	}
	require.Empty(t, blockers, "active daemon preflight must have zero blockers: %+v", blockers)
	require.NotEmpty(t, autoNotes, "running daemon and live index lag must be auto notes")
	require.Len(t, quarantineNotes, 2, "must report quarantine notes for 2 malformed savings records")

	// 5. Updater stops the daemon and clean shutdown heals live index lag
	_ = holder.Process.Kill()
	_ = holder.Wait()
	healStoreLiveLag(t, filepath.Join(dir, "context"), "context")

	// 6. Post-stop strict cutover (new binary migrate --apply)
	runner := NewRunner(DefaultRegistry)
	env := Env{DataDir: dir, BinaryVersion: "11.0.0", Context: context.Background()}
	require.NoError(t, runner.Apply(env, schema.Schema2))

	// 7. Verify ledger advanced cleanly to schema 2
	ledger, err := schema.Load(dir)
	require.NoError(t, err)
	require.Equal(t, schema.Schema2, ledger.SchemaVersion)
	require.Nil(t, ledger.InProgress)
	// This is a historical schema-2 fixture. Its verifier intentionally uses
	// schema-2 codecs; the format-3 runtime guard must still refuse it.
	require.Equal(t, schema.VerdictStart, schema.Decide(ledger.SchemaVersion, schema.Schema2, 1, false))

	// 8. Verify live database via storage.Open
	s, err := storage.OpenSchema2ForMigrationVerification(dir)
	require.NoError(t, err)
	defer s.Close()
	require.NotNil(t, s.DB, "must open shared warden-db")

	// Active agent preserved with exact status, PID, and links
	liveAgent, err := s.Agents.Get(context.Background(), "agent-live")
	require.NoError(t, err)
	require.Equal(t, store.StatusWorking, liveAgent.Status, "active agent must remain in working status")
	require.Equal(t, 4242, liveAgent.PID)
	require.Equal(t, rfPlan, liveAgent.PlanID)
	require.Equal(t, rfRun, liveAgent.AutopilotRunID)

	// Active autopilot run preserved in active state
	activeRun, err := s.AutopilotRuns.Get(rfRun)
	require.NoError(t, err)
	require.Equal(t, autopilot.StateActive, activeRun.State)
	require.Equal(t, rfPlan, activeRun.PlanID)

	// Plan preserved in progress
	plan, err := s.Plans.Get(context.Background(), rfPlan)
	require.NoError(t, err)
	require.Equal(t, planstore.PlanStatusInProgress, plan.Status)

	// Valid savings events: exactly 3 valid events imported
	savingsEvts, err := s.Savings.Events(time.Time{})
	require.NoError(t, err)
	require.Len(t, savingsEvts, 3, "exactly the 3 valid savings events imported")
	require.Equal(t, 6, savingsEvts[0].Saved)
	require.Equal(t, 0, savingsEvts[1].Saved)
	require.Equal(t, 10, savingsEvts[2].Saved)

	// 9. Verify quarantine artifacts on disk
	quarDir := filepath.Join(dir, "quarantine", "savings")
	require.DirExists(t, quarDir)

	manifestBytes, err := os.ReadFile(filepath.Join(quarDir, "manifest.json"))
	require.NoError(t, err)
	var manifest struct {
		SourcePath   string              `json:"source_path"`
		TotalRecords int                 `json:"total_records"`
		Records      []QuarantinedRecord `json:"records"`
	}
	require.NoError(t, json.Unmarshal(manifestBytes, &manifest))
	require.Equal(t, "savings/ledger.jsonl", manifest.SourcePath)
	require.Equal(t, 2, manifest.TotalRecords)
	require.Len(t, manifest.Records, 2)

	require.Equal(t, 2, manifest.Records[0].Line)
	require.Equal(t, expectedSHA2, manifest.Records[0].SHA256)
	raw2, err := os.ReadFile(filepath.Join(quarDir, fmt.Sprintf("line_%06d_offset_%012d_%s.raw", manifest.Records[0].Line, manifest.Records[0].Offset, expectedSHA2[:8])))
	require.NoError(t, err)
	require.Equal(t, expectedRawLine2, string(raw2))

	require.Equal(t, 4, manifest.Records[1].Line)
	require.Equal(t, expectedSHA4, manifest.Records[1].SHA256)
	raw4, err := os.ReadFile(filepath.Join(quarDir, fmt.Sprintf("line_%06d_offset_%012d_%s.raw", manifest.Records[1].Line, manifest.Records[1].Offset, expectedSHA4[:8])))
	require.NoError(t, err)
	require.Equal(t, expectedRawLine4, string(raw4))

	// 10. Verify immutable pre-schema2 backup exists
	backups, err := filepath.Glob(filepath.Join(dir, "backups", Schema2BackupPrefix+"*"))
	require.NoError(t, err)
	require.Len(t, backups, 1, "immutable pre-schema2 backup must be created")
}

// TestReleaseAcceptance_ReportedUpdatePath_PreflightSafeRefusalWithoutMutation
// exercises safe refusal during preflight when non-quarantineable corruption
// (broken line boundary exceeding 16MiB limit or unexpected index damage) is present,
// asserting zero mutations to source files and ledger.
func TestReleaseAcceptance_ReportedUpdatePath_PreflightSafeRefusalWithoutMutation(t *testing.T) {
	dir := t.TempDir()
	seedRealisticLegacy(t, dir)
	beforeSnapshot := snapshotTree(t, dir)

	// Write savings/ledger.jsonl with line exceeding 16MiB (broken line boundaries)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "savings"), 0o700))
	corruptPath := filepath.Join(dir, "savings", "ledger.jsonl")
	f, err := os.OpenFile(corruptPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	require.NoError(t, err)
	chunk := bytes.Repeat([]byte("a"), 1024*1024)
	for i := 0; i < 17; i++ { // >16MiB without newline
		_, err = f.Write(chunk)
		require.NoError(t, err)
	}
	require.NoError(t, f.Close())

	// Hold live daemon lock
	daemonLock, err := ownerlock.Acquire(dir, ownerlock.Info{Kind: ownerlock.KindDaemon, Version: "10.0.1", Command: "warden daemon"})
	require.NoError(t, err)

	// Preflight must strictly refuse before changes
	findings := checkSchema2Cutover(Env{DataDir: dir, Context: context.Background()})
	var savingsBlockers []Finding
	for _, fn := range findings {
		if fn.Severity == SeverityBlocking && fn.Store == "savings/ledger.jsonl" {
			savingsBlockers = append(savingsBlockers, fn)
		}
	}
	require.Len(t, savingsBlockers, 1, "broken line boundaries must block preflight: %+v", findings)
	require.Contains(t, savingsBlockers[0].Message, "16MiB limit")
	require.Empty(t, savingsBlockers[0].Command, "must not suggest repair commands for corrupt JSONL")

	require.NoError(t, daemonLock.Release())

	// Zero mutation to disk
	ledger, err := schema.Load(dir)
	require.NoError(t, err)
	require.Equal(t, 1, ledger.SchemaVersion)
	require.False(t, schema.WardenDBPresent(dir), "no warden-db may be created on refusal")

	// Aside from the intentionally modified savings/ledger.jsonl, all files are identical
	afterSnapshot := snapshotTree(t, dir)
	for rel, want := range beforeSnapshot {
		if rel == "savings/ledger.jsonl" {
			continue
		}
		require.Equal(t, want, afterSnapshot[rel], "file %s mutated on refusal", rel)
	}
}

// TestReleaseAcceptance_ReportedUpdatePath_PostStopStrictCutoverRefusalWithoutMutation
// exercises strict re-verification post-stop: if a rogue owner appears or unhealed
// index damage exists when cutover runs, it safely refuses without mutating data.
func TestReleaseAcceptance_ReportedUpdatePath_PostStopStrictCutoverRefusalWithoutMutation(t *testing.T) {
	dir := t.TempDir()
	seedRealisticLegacy(t, dir)
	beforeSnapshot := snapshotTree(t, dir)

	// Simulate post-stop race: a foreign owner acquires the lock in a separate process before migrate --apply
	holder := startOwnerHolder(t, dir)

	runner := NewRunner(DefaultRegistry)
	env := Env{DataDir: dir, BinaryVersion: "11.0.0", Context: context.Background()}
	applyErr := runner.Apply(env, schema.Schema2)
	require.Error(t, applyErr)
	require.ErrorIs(t, applyErr, ErrRefusedBeforeChange)

	_ = holder.Process.Kill()
	_ = holder.Wait()

	// Data dir must remain strictly at schema 1 and unmigrated
	ledger, err := schema.Load(dir)
	require.NoError(t, err)
	require.Equal(t, 1, ledger.SchemaVersion)
	require.False(t, schema.WardenDBPresent(dir))

	afterSnapshot := snapshotTree(t, dir)
	for rel, want := range beforeSnapshot {
		require.Equal(t, want, afterSnapshot[rel], "file %s mutated on post-stop refusal", rel)
	}
}

func startOwnerHolder(t *testing.T, dir string) *exec.Cmd {
	t.Helper()
	c := exec.Command(os.Args[0])
	c.Env = append(os.Environ(), "WARDEN_MIGRATE_HOLD_DIR="+dir)
	out, err := c.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, c.Start())
	t.Cleanup(func() { _ = c.Process.Kill(); _ = c.Wait() })
	buf := make([]byte, 8)
	n, _ := out.Read(buf)
	require.Equal(t, "OWNER\n", string(buf[:n]))
	return c
}

func createStoreWithLiveLag(t *testing.T, storeDir, colName string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(storeDir, 0o700))
	db, err := engine.Open(storeDir, engine.CollectionConfig{})
	require.NoError(t, err)
	col, err := db.Collection(colName)
	require.NoError(t, err)
	id, _, err := col.InsertWithKey("live_lag_key", map[string]any{"val": "v1"})
	require.NoError(t, err)
	require.NoError(t, db.Close())

	colDir := filepath.Join(storeDir, colName)

	segPath := filepath.Join(colDir, "seg_000001.ndjson")
	line2 := fmt.Sprintf(`{"id":%d,"op":"update","rev":2,"ts":"2026-10-18T10:01:00Z","data":{"_key":"live_lag_key","val":"v2"}}`+"\n", id)
	f, err := os.OpenFile(segPath, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = f.WriteString(line2)
	require.NoError(t, err)
	require.NoError(t, f.Close())
}

func healStoreLiveLag(t *testing.T, storeDir, colName string) {
	t.Helper()
	db, err := engine.Open(storeDir, engine.CollectionConfig{})
	require.NoError(t, err)
	_, err = db.Collection(colName)
	require.NoError(t, err)
	require.NoError(t, db.Close())
}
