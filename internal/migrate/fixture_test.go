package migrate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/warden/internal/schema"
	"github.com/stretchr/testify/require"
)

// TestV925ToV928FailurePreflightCatchesWithZeroDowntime reproduces the incident
// from docs/specs/2026-10-09-update-process.md §1:
// Upgrading from v9.25.0 to v9.28.0 failed repeatedly because latent revision-regression
// conflicts and duplicate live keys in context, backends, agents-db/closed, inbox/messages,
// plans/* and projects turned into boot failures when strict integrity checking was enabled.
//
// This test asserts that whole-store preflight checks catch all these latent conflicts
// before any daemon restart or mutation occurs, classifying them as repairable and providing
// the exact suggested repair command.
func TestV925ToV928FailurePreflightCatchesWithZeroDowntime(t *testing.T) {
	dataDir := t.TempDir()

	// Initial ledger at schema 1
	l := &schema.Ledger{
		SchemaVersion: 1,
		BinaryVersion: "9.25.0",
		History: []schema.HistoryEntry{{
			From:      0,
			To:        1,
			Migration: schema.MigrationBaselineLegacy,
		}},
	}
	require.NoError(t, schema.Save(dataDir, l))

	// Fixture stores matching the incident:
	// context, backends, agents-db, inbox, plans/plans-db, projects
	stores := []struct {
		relPath string
		col     string
	}{
		{"context", "context"},
		{"backends", "backends"},
		{"agents-db", "closed"},
		{"inbox", "messages"},
		{"plans/plans-db", "plans"},
		{"projects", "projects"},
	}

	for _, s := range stores {
		colDir := filepath.Join(dataDir, filepath.FromSlash(s.relPath), s.col)
		require.NoError(t, os.MkdirAll(colDir, 0o700))

		// Write initial record, higher revision update, then a regressed revision update
		// followed by a duplicate key on a different record id.
		segPath := filepath.Join(colDir, "seg_000001.ndjson")
		lines := fmt.Sprintf(
			"{\"id\":1,\"op\":\"insert\",\"rev\":1,\"ts\":\"2026-10-09T10:00:00.000Z\",\"data\":{\"_key\":\"key-1\",\"val\":\"v1\"}}\n" +
				"{\"id\":1,\"op\":\"update\",\"rev\":3,\"ts\":\"2026-10-09T10:01:00.000Z\",\"data\":{\"_key\":\"key-1\",\"val\":\"v3\"}}\n" +
				"{\"id\":1,\"op\":\"update\",\"rev\":2,\"ts\":\"2026-10-09T10:02:00.000Z\",\"data\":{\"_key\":\"key-1\",\"val\":\"v2-regression\"}}\n" +
				"{\"id\":2,\"op\":\"insert\",\"rev\":1,\"ts\":\"2026-10-09T10:03:00.000Z\",\"data\":{\"_key\":\"key-1\",\"val\":\"duplicate-key\"}}\n",
		)
		require.NoError(t, os.WriteFile(segPath, []byte(lines), 0o600))
	}

	// Capture initial timestamps/contents to verify zero mutation
	beforeSnapshot := make(map[string][]byte)
	for _, s := range stores {
		segPath := filepath.Join(dataDir, filepath.FromSlash(s.relPath), s.col, "seg_000001.ndjson")
		b, err := os.ReadFile(segPath)
		require.NoError(t, err)
		beforeSnapshot[segPath] = b
	}

	// Run whole-store check via Runner.Check
	rn := NewRunner(nil)
	env := Env{
		DataDir:       dataDir,
		BinaryVersion: "9.28.0",
		Context:       context.Background(),
	}

	findings, err := rn.Check(env, 1)
	require.NoError(t, err)
	require.NotEmpty(t, findings)

	// Verify all affected stores are detected and classified as SeverityRepairable
	findingsByStore := make(map[string][]Finding)
	for _, f := range findings {
		findingsByStore[f.Store] = append(findingsByStore[f.Store], f)
	}

	for _, s := range stores {
		// Store name in finding is relPath/col (or relPath)
		targetKey := s.relPath + "/" + s.col
		if s.relPath == "backends" {
			targetKey = "backends/" + s.col
		}

		storeFindings, found := findingsByStore[targetKey]
		require.True(t, found, "expected findings for store %s", targetKey)
		require.NotEmpty(t, storeFindings)

		hasRepairable := false
		for _, f := range storeFindings {
			if f.Severity == SeverityRepairable {
				hasRepairable = true
				require.NotEmpty(t, f.Command, "repairable finding must specify a repair command")
				switch s.relPath {
				case "backends":
					require.Equal(t, "warden repair backends", f.Command)
				case "agents-db":
					require.Equal(t, "warden repair agents", f.Command)
				default:
					require.Equal(t, "warden repair all --resolve-history=live-wins", f.Command)
				}
			}
		}
		require.True(t, hasRepairable, "store %s must report SeverityRepairable findings", targetKey)
	}

	// Verify zero mutation occurred on any of the segment files
	for segPath, want := range beforeSnapshot {
		got, err := os.ReadFile(segPath)
		require.NoError(t, err)
		require.Equal(t, want, got, "segment file %s must remain untouched", segPath)
	}

	// Verify engine verification directly agrees on conflicts
	for _, s := range stores {
		fullPath := filepath.Join(dataDir, filepath.FromSlash(s.relPath))
		ir, err := engine.VerifyDir(context.Background(), fullPath, engine.VerifyOptions{Mode: engine.VerifyFull})
		require.NoError(t, err)
		require.False(t, ir.Clean())
		require.True(t, ir.Has(engine.CodeConflictRevision) || ir.Has(engine.CodeConflictDuplicateID))
	}
}
