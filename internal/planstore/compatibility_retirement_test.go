package planstore

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// Compatibility retirement (Phase 11): old plan YAML files and old scan callers
// remain usable as migration aids, but cannot reseed Status or redefine
// execution for Plans that already have a canonical definition.

func TestScanProject_doesNotReseedStatusAfterCanonicalImport(t *testing.T) {
	svc, store, root, _ := newTestService(t)
	ctx := context.Background()
	projectID := "proj-compat"

	writeLegacyPlan(t, root, "plans/pending/ship-it.yaml", legacyYAML("ship-it", "ship the feature",
		"  - id: t1\n    prompt: implement\n  - id: t2\n    prompt: review\n    after: [t1]\n"))

	report, err := svc.ImportLegacy(ctx, projectID, ImportOptions{})
	require.NoError(t, err)
	require.Len(t, report.Imported, 1)

	id := PlanID(projectID, "ship-it")
	before, err := store.Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, PlanStatusPending, before.Status)
	require.Equal(t, "ship the feature", before.Goal)
	require.NotEmpty(t, before.ContentHash)

	// Operator moves the replica YAML (inert) into in_progress/ — must not flip Status.
	require.NoError(t, os.Remove(filepath.Join(root, "plans/pending/ship-it.yaml")))
	writeLegacyPlan(t, root, "plans/in_progress/ship-it.yaml", legacyYAML("ship-it", "hijacked goal from disk",
		"  - id: t1\n    prompt: implement\n"))

	scanRes, err := ScanProject(ctx, store, projectID, root)
	require.NoError(t, err)
	require.Equal(t, 0, scanRes.Upserted)
	require.Equal(t, 1, scanRes.SkippedCanonical)
	require.Equal(t, ScanDeprecationNotice, scanRes.Notice)

	after, err := store.Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, PlanStatusPending, after.Status, "scan must not reseed Status after import")
	require.Equal(t, before.Goal, after.Goal, "scan must not redefine Goal from YAML")
	require.Equal(t, before.ContentHash, after.ContentHash)
	require.Equal(t, before.Revision, after.Revision)
	require.Equal(t, "plans/in_progress/ship-it.yaml", after.FilePath, "FilePath may refresh as last-export metadata")
}

func TestScanProject_oldCallerStillCreatesStubs(t *testing.T) {
	// Old callers that only invoke scan (pre-import-legacy) still get stub records.
	s := newTestStore(t)
	ctx := context.Background()
	root := t.TempDir()

	writePlanFile(t, root, "plans/completed/legacy-done.yaml", "legacy-done")
	res, err := ScanProject(ctx, s, "proj-old", root)
	require.NoError(t, err)
	require.Equal(t, 1, res.Upserted)
	require.Equal(t, 0, res.SkippedCanonical)
	require.Contains(t, res.Notice, "deprecated")

	got, err := s.Get(ctx, PlanID("proj-old", "legacy-done"))
	require.NoError(t, err)
	require.Equal(t, PlanStatusCompleted, got.Status)
	require.True(t, isEmptyDefinition(got), "scan creates stubs without definition")
}

func TestImportLegacy_oldPlanFileCorpus(t *testing.T) {
	// Representative pre-cutover YAML shapes across all four lifecycle dirs + flat.
	svc, store, root, _ := newTestService(t)
	ctx := context.Background()
	projectID := "proj-corpus"

	writeLegacyPlan(t, root, "plans/pending/p.yaml", legacyYAML("pending-plan", "g-p",
		"  - id: a\n    prompt: do a\n"))
	writeLegacyPlan(t, root, "plans/in_progress/i.yaml", legacyYAML("inprog-plan", "g-i",
		"  - id: b\n    prompt: do b\n"))
	writeLegacyPlan(t, root, "plans/completed/c.yaml", legacyYAML("done-plan", "g-c",
		"  - id: c\n    prompt: do c\n"))
	writeLegacyPlan(t, root, "plans/archived/a.yaml", legacyYAML("arch-plan", "g-a",
		"  - id: d\n    prompt: do d\n"))
	writeLegacyPlan(t, root, "plans/flat.yaml", legacyYAML("flat-plan", "g-f",
		"  - id: e\n    prompt: do e\n"))

	report, err := svc.ImportLegacy(ctx, projectID, ImportOptions{})
	require.NoError(t, err)
	imported, skipped, conflicted, errored := report.Counts()
	require.Equal(t, 5, imported)
	require.Equal(t, 0, skipped)
	require.Equal(t, 0, conflicted)
	require.Equal(t, 0, errored)

	// Second import with matching hashes is a pure skip (old caller re-run).
	report2, err := svc.ImportLegacy(ctx, projectID, ImportOptions{})
	require.NoError(t, err)
	_, skipped2, _, _ := report2.Counts()
	require.Equal(t, 5, skipped2)
	require.Empty(t, report2.Imported)

	pending, err := store.Get(ctx, PlanID(projectID, "pending-plan"))
	require.NoError(t, err)
	require.Equal(t, PlanStatusPending, pending.Status)
	require.Equal(t, "g-p", pending.Goal)

	flat, err := store.Get(ctx, PlanID(projectID, "flat-plan"))
	require.NoError(t, err)
	require.Equal(t, PlanStatusPending, flat.Status)
}
