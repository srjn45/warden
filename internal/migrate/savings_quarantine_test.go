package migrate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/schema"
	"github.com/srjn45/warden/internal/storage"
)

func TestSavingsQuarantine_MiddleAndValidRowsAroundIt(t *testing.T) {
	dir := t.TempDir()
	seedRealisticLegacy(t, dir)

	// Valid row 1, malformed middle row 2, valid row 3
	line1 := `{"ts":"2026-10-17T09:00:00Z","feature":"compact","raw_tokens":10,"kept_tokens":4,"saved":6,"agent":"agent-live"}` + "\n"
	line2 := `not json at all middle corruption {"broken":` + "\n"
	line3 := `{"ts":"2026-10-17T09:05:00Z","feature":"classify","raw_tokens":5,"kept_tokens":5,"saved":0,"agent":"agent-live"}` + "\n"
	writeFile(t, dir, "savings/ledger.jsonl", line1+line2+line3)

	expectedOffset := int64(len(line1))
	expectedRaw := strings.TrimRight(line2, "\r\n")
	sum := sha256.Sum256([]byte(expectedRaw))
	expectedSHA := hex.EncodeToString(sum[:])

	// 1. Preflight check: should not block, but report a repairable finding
	p, err := PreflightSchema2(context.Background(), dir, time.Now())
	require.NoError(t, err)
	require.False(t, p.Blocked, "malformed savings lines must not block preflight: %+v", p.Findings)

	var repairableFindings []Finding
	for _, f := range p.Findings {
		if f.Store == "savings/ledger.jsonl" && f.Severity == SeverityRepairable {
			repairableFindings = append(repairableFindings, f)
		}
	}
	require.Len(t, repairableFindings, 1)
	require.Contains(t, repairableFindings[0].Message, "line 2")
	require.Contains(t, repairableFindings[0].Message, fmt.Sprintf("offset %d", expectedOffset))
	require.Contains(t, repairableFindings[0].Message, expectedSHA[:12])

	// 2. Cutover Apply: should succeed
	rn := NewRunner(DefaultRegistry)
	env := Env{DataDir: dir, BinaryVersion: "11.0.0", Context: context.Background()}
	require.NoError(t, rn.Apply(env, schema.Schema2))

	// 3. Verify ledger advanced
	l, err := schema.Load(dir)
	require.NoError(t, err)
	require.Equal(t, schema.Schema2, l.SchemaVersion)

	// 4. Verify live database via storage.Open
	st, err := storage.OpenSchema2ForMigrationVerification(dir)
	require.NoError(t, err)
	evts, err := st.Savings.Events(time.Time{})
	require.NoError(t, err)
	require.Len(t, evts, 2, "exactly 2 valid events imported into savings_events")
	require.Equal(t, "compact", evts[0].Feature)
	require.Equal(t, 6, evts[0].Saved)
	require.Equal(t, "classify", evts[1].Feature)
	require.Equal(t, 0, evts[1].Saved)
	require.NoError(t, st.Close())

	// 5. Verify quarantine on disk
	quarDir := filepath.Join(dir, "quarantine", "savings")
	require.DirExists(t, quarDir)

	// Check manifest.json
	manifestBytes, err := os.ReadFile(filepath.Join(quarDir, "manifest.json"))
	require.NoError(t, err)
	var manifest struct {
		SourcePath   string              `json:"source_path"`
		TotalRecords int                 `json:"total_records"`
		Records      []QuarantinedRecord `json:"records"`
	}
	require.NoError(t, json.Unmarshal(manifestBytes, &manifest))
	require.Equal(t, "savings/ledger.jsonl", manifest.SourcePath)
	require.Equal(t, 1, manifest.TotalRecords)
	require.Len(t, manifest.Records, 1)

	qRec := manifest.Records[0]
	require.Equal(t, "savings/ledger.jsonl", qRec.SourcePath)
	require.Equal(t, 2, qRec.Line)
	require.Equal(t, expectedOffset, qRec.Offset)
	require.Equal(t, len(expectedRaw), qRec.Length)
	require.Equal(t, expectedRaw, qRec.RawBytes)
	require.Equal(t, hex.EncodeToString([]byte(expectedRaw)), qRec.RawHex)
	require.Equal(t, expectedSHA, qRec.SHA256)
	require.NotEmpty(t, qRec.Reason)

	// Check records.jsonl
	recordsBytes, err := os.ReadFile(filepath.Join(quarDir, "records.jsonl"))
	require.NoError(t, err)
	var qLine QuarantinedRecord
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(recordsBytes), &qLine))
	require.Equal(t, qRec, qLine)

	// Check standalone raw file
	rawFileName := fmt.Sprintf("line_%06d_offset_%012d_%s.raw", 2, expectedOffset, expectedSHA[:8])
	rawFilePath := filepath.Join(quarDir, rawFileName)
	rawOnDisk, err := os.ReadFile(rawFilePath)
	require.NoError(t, err)
	require.Equal(t, []byte(expectedRaw), rawOnDisk, "raw bytes on disk match byte-for-byte")
}

func TestSavingsQuarantine_TrailingRowWithoutNewline(t *testing.T) {
	dir := t.TempDir()
	seedRealisticLegacy(t, dir)

	line1 := `{"ts":"2026-10-17T09:00:00Z","feature":"compact","raw_tokens":10,"kept_tokens":4,"saved":6,"agent":"agent-live"}` + "\n"
	line2 := `{"ts":"2026-10-17T09:05:00Z","feature":"classify","raw_tokens":5,"kept_tokens":5,"saved":0,"agent":"agent-live"}` + "\n"
	line3 := `truncated trailing row without newline {"ts":`
	writeFile(t, dir, "savings/ledger.jsonl", line1+line2+line3)

	expectedOffset := int64(len(line1) + len(line2))
	expectedRaw := line3
	sum := sha256.Sum256([]byte(expectedRaw))
	expectedSHA := hex.EncodeToString(sum[:])

	p, err := PreflightSchema2(context.Background(), dir, time.Now())
	require.NoError(t, err)
	require.False(t, p.Blocked)

	rn := NewRunner(DefaultRegistry)
	env := Env{DataDir: dir, BinaryVersion: "11.0.0", Context: context.Background()}
	require.NoError(t, rn.Apply(env, schema.Schema2))

	st, err := storage.OpenSchema2ForMigrationVerification(dir)
	require.NoError(t, err)
	evts, err := st.Savings.Events(time.Time{})
	require.NoError(t, err)
	require.Len(t, evts, 2)
	require.NoError(t, st.Close())

	// Verify quarantine
	quarDir := filepath.Join(dir, "quarantine", "savings")
	manifestBytes, err := os.ReadFile(filepath.Join(quarDir, "manifest.json"))
	require.NoError(t, err)
	var manifest struct {
		Records []QuarantinedRecord `json:"records"`
	}
	require.NoError(t, json.Unmarshal(manifestBytes, &manifest))
	require.Len(t, manifest.Records, 1)
	require.Equal(t, 3, manifest.Records[0].Line)
	require.Equal(t, expectedOffset, manifest.Records[0].Offset)
	require.Equal(t, expectedRaw, manifest.Records[0].RawBytes)
	require.Equal(t, expectedSHA, manifest.Records[0].SHA256)
}

func TestSavingsQuarantine_MiddleAndTrailingRowsWithValidAroundThem(t *testing.T) {
	dir := t.TempDir()
	seedRealisticLegacy(t, dir)

	line1 := `{"ts":"2026-10-17T09:00:00Z","feature":"compact","raw_tokens":10,"kept_tokens":4,"saved":6,"agent":"agent-live"}` + "\n"
	line2 := `middle corruption row 2` + "\n"
	line3 := `{"ts":"2026-10-17T09:05:00Z","feature":"classify","raw_tokens":5,"kept_tokens":5,"saved":0,"agent":"agent-live"}` + "\n"
	line4 := `trailing corruption row 4` + "\n"
	writeFile(t, dir, "savings/ledger.jsonl", line1+line2+line3+line4)

	rn := NewRunner(DefaultRegistry)
	env := Env{DataDir: dir, BinaryVersion: "11.0.0", Context: context.Background()}
	require.NoError(t, rn.Apply(env, schema.Schema2))

	st, err := storage.OpenSchema2ForMigrationVerification(dir)
	require.NoError(t, err)
	evts, err := st.Savings.Events(time.Time{})
	require.NoError(t, err)
	require.Len(t, evts, 2, "both valid rows (line 1 and line 3) imported")
	require.Equal(t, "compact", evts[0].Feature)
	require.Equal(t, "classify", evts[1].Feature)
	require.NoError(t, st.Close())

	quarDir := filepath.Join(dir, "quarantine", "savings")
	manifestBytes, err := os.ReadFile(filepath.Join(quarDir, "manifest.json"))
	require.NoError(t, err)
	var manifest struct {
		TotalRecords int                 `json:"total_records"`
		Records      []QuarantinedRecord `json:"records"`
	}
	require.NoError(t, json.Unmarshal(manifestBytes, &manifest))
	require.Equal(t, 2, manifest.TotalRecords)
	require.Len(t, manifest.Records, 2)
	require.Equal(t, 2, manifest.Records[0].Line)
	require.Equal(t, "middle corruption row 2", manifest.Records[0].RawBytes)
	require.Equal(t, 4, manifest.Records[1].Line)
	require.Equal(t, "trailing corruption row 4", manifest.Records[1].RawBytes)
}

func TestSavingsQuarantine_InterruptionAndResume(t *testing.T) {
	dir := t.TempDir()
	seedRealisticLegacy(t, dir)

	line1 := `{"ts":"2026-10-17T09:00:00Z","feature":"compact","raw_tokens":10,"kept_tokens":4,"saved":6,"agent":"agent-live"}` + "\n"
	line2 := `middle corruption row` + "\n"
	line3 := `{"ts":"2026-10-17T09:05:00Z","feature":"classify","raw_tokens":5,"kept_tokens":5,"saved":0,"agent":"agent-live"}` + "\n"
	writeFile(t, dir, "savings/ledger.jsonl", line1+line2+line3)

	rn := NewRunner(DefaultRegistry)
	env := Env{DataDir: dir, BinaryVersion: "11.0.0", Context: context.Background()}

	// First run: apply cutover
	require.NoError(t, rn.Apply(env, schema.Schema2))
	done, err := schema.Load(dir)
	require.NoError(t, err)
	backup := done.History[len(done.History)-1].Backup
	require.NotEmpty(t, backup)

	// Simulate crash: schema 1 + journal at "promoted" + warden-db in place
	require.NoError(t, schema.Save(dir, &schema.Ledger{
		SchemaVersion: 1,
		BinaryVersion: "10.0.1",
		InProgress:    &schema.InProgress{Migration: Migration0002ID, Step: "promoted", Snapshot: backup},
	}))

	// Resume after crash
	require.NoError(t, rn.Resume(env))

	l, err := schema.Load(dir)
	require.NoError(t, err)
	require.Equal(t, schema.Schema2, l.SchemaVersion)
	require.Nil(t, l.InProgress)

	// Verify valid events in warden-db
	st, err := storage.OpenSchema2ForMigrationVerification(dir)
	require.NoError(t, err)
	evts, err := st.Savings.Events(time.Time{})
	require.NoError(t, err)
	require.Len(t, evts, 2)
	require.NoError(t, st.Close())

	// Verify quarantine remains intact and auditable
	quarDir := filepath.Join(dir, "quarantine", "savings")
	manifestBytes, err := os.ReadFile(filepath.Join(quarDir, "manifest.json"))
	require.NoError(t, err)
	var manifest struct {
		TotalRecords int                 `json:"total_records"`
		Records      []QuarantinedRecord `json:"records"`
	}
	require.NoError(t, json.Unmarshal(manifestBytes, &manifest))
	require.Equal(t, 1, manifest.TotalRecords)
	require.Equal(t, "middle corruption row", manifest.Records[0].RawBytes)
}

func TestSavingsQuarantine_BackupAndRestoreIsReversible(t *testing.T) {
	dir := t.TempDir()
	seedRealisticLegacy(t, dir)

	line1 := `{"ts":"2026-10-17T09:00:00Z","feature":"compact","raw_tokens":10,"kept_tokens":4,"saved":6,"agent":"agent-live"}` + "\n"
	line2 := `middle corruption row` + "\n"
	line3 := `{"ts":"2026-10-17T09:05:00Z","feature":"classify","raw_tokens":5,"kept_tokens":5,"saved":0,"agent":"agent-live"}` + "\n"
	originalContent := line1 + line2 + line3
	writeFile(t, dir, "savings/ledger.jsonl", originalContent)

	rn := NewRunner(DefaultRegistry)
	env := Env{DataDir: dir, BinaryVersion: "11.0.0", Context: context.Background()}

	// Apply cutover
	require.NoError(t, rn.Apply(env, schema.Schema2))
	done, err := schema.Load(dir)
	require.NoError(t, err)
	backup := done.History[len(done.History)-1].Backup
	require.NotEmpty(t, backup)

	// Simulate crash state for restore: schema 1 with in_progress snapshot pointing to backup
	require.NoError(t, schema.Save(dir, &schema.Ledger{
		SchemaVersion: 1,
		BinaryVersion: "10.0.1",
		InProgress:    &schema.InProgress{Migration: Migration0002ID, Step: "promoted", Snapshot: backup},
	}))

	// Execute Restore (rollback)
	require.NoError(t, rn.Restore(env))

	l, err := schema.Load(dir)
	require.NoError(t, err)
	require.Equal(t, 1, l.SchemaVersion)
	require.Nil(t, l.InProgress)
	require.False(t, schema.WardenDBPresent(dir), "warden-db must be moved out on restore")

	// Restored savings/ledger.jsonl must match original legacy bytes exactly
	restoredContent, err := os.ReadFile(filepath.Join(dir, "savings", "ledger.jsonl"))
	require.NoError(t, err)
	require.Equal(t, originalContent, string(restoredContent), "restored file matches original exactly")

	// Opens on legacy schema 1 stores
	legacy, err := storage.OpenLegacy(dir)
	require.NoError(t, err)
	legEvts, err := legacy.Savings.Events(time.Time{})
	require.NoError(t, err)
	require.Len(t, legEvts, 2, "legacy store skips malformed line and returns 2 events")
	require.NoError(t, legacy.Close())

	// Re-apply cutover after restore
	time.Sleep(1100 * time.Millisecond) // backup directory timestamp is second-resolution
	require.NoError(t, rn.Apply(env, schema.Schema2))

	l2, err := schema.Load(dir)
	require.NoError(t, err)
	require.Equal(t, schema.Schema2, l2.SchemaVersion)

	st, err := storage.OpenSchema2ForMigrationVerification(dir)
	require.NoError(t, err)
	evts, err := st.Savings.Events(time.Time{})
	require.NoError(t, err)
	require.Len(t, evts, 2)
	require.NoError(t, st.Close())
}

func TestSavingsQuarantine_CorruptedBoundaryBlocksWithGuidance(t *testing.T) {
	dir := t.TempDir()
	seedRealisticLegacy(t, dir)

	// Create an un-delimited row exceeding the 16MiB limit
	hugeRow := make([]byte, 17*1024*1024)
	for i := range hugeRow {
		hugeRow[i] = 'A'
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "savings", "ledger.jsonl"), hugeRow, 0o600))
	before := snapshotTree(t, dir)

	// Preflight must block
	p, err := PreflightSchema2(context.Background(), dir, time.Now())
	require.NoError(t, err)
	require.True(t, p.Blocked, "unrecoverable corruption must block preflight")

	var blockingFindings []Finding
	for _, f := range p.Findings {
		if f.Store == "savings/ledger.jsonl" && f.Severity == SeverityBlocking {
			blockingFindings = append(blockingFindings, f)
		}
	}
	require.NotEmpty(t, blockingFindings, "expected blocking finding for corrupted boundary")
	require.Equal(t, string(ClassCorruption), blockingFindings[0].Class)
	require.Contains(t, blockingFindings[0].Message, "16MiB")
	require.Contains(t, blockingFindings[0].Details, "restore savings/ledger.jsonl from backup")

	// Cutover must refuse and change no source byte
	rn := NewRunner(DefaultRegistry)
	env := Env{DataDir: dir, BinaryVersion: "11.0.0", Context: context.Background()}
	err = rn.Apply(env, schema.Schema2)
	require.Error(t, err)
	require.False(t, schema.WardenDBPresent(dir))
	require.Equal(t, before, snapshotTree(t, dir), "refused cutover must not alter data dir")
}
