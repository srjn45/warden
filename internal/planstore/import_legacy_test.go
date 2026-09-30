package planstore

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func writeLegacyPlan(t *testing.T, root, rel, body string) {
	t.Helper()
	abs := filepath.Join(root, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o755))
	require.NoError(t, os.WriteFile(abs, []byte(body), 0o644))
}

func legacyYAML(name, goal string, tasks string) string {
	return "version: 1\nname: " + name + "\ngoal: " + goal + "\ntasks:\n" + tasks
}

func TestImportLegacy_allFourStatusesAndDAG(t *testing.T) {
	svc, store, root, _ := newTestService(t)
	ctx := context.Background()
	projectID := "proj-legacy"

	writeLegacyPlan(t, root, "plans/pending/feat-pending.yaml", legacyYAML("feat-pending", "ship pending",
		"  - id: t1\n    prompt: do pending\n"))
	writeLegacyPlan(t, root, "plans/in_progress/feat-wip.yaml", legacyYAML("feat-wip", "ship wip",
		"  - id: a\n    prompt: first\n  - id: b\n    prompt: second\n    after: [a]\n    status: in_progress\n"))
	writeLegacyPlan(t, root, "plans/completed/feat-done.yaml", legacyYAML("feat-done", "ship done",
		"  - id: t1\n    prompt: finished\n    status: done\n"))
	writeLegacyPlan(t, root, "plans/archived/feat-old.yaml", legacyYAML("feat-old", "ship archived",
		"  - id: t1\n    prompt: archived work\n"))

	report, err := svc.ImportLegacy(ctx, projectID, ImportOptions{})
	require.NoError(t, err)
	imported, skipped, conflicted, errored := report.Counts()
	require.Equal(t, 4, imported)
	require.Equal(t, 0, skipped)
	require.Equal(t, 0, conflicted)
	require.Equal(t, 0, errored)

	pending, err := store.Get(ctx, PlanID(projectID, "feat-pending"))
	require.NoError(t, err)
	require.Equal(t, PlanStatusPending, pending.Status)
	require.Equal(t, "ship pending", pending.Goal)
	require.Nil(t, pending.StartedAt)
	require.Nil(t, pending.CompletedAt)

	wip, err := store.Get(ctx, PlanID(projectID, "feat-wip"))
	require.NoError(t, err)
	require.Equal(t, PlanStatusInProgress, wip.Status)
	require.Len(t, wip.Tasks, 2)
	require.Equal(t, []string{"a"}, wip.Tasks[1].After)
	require.Equal(t, "in_progress", wip.TaskProgress["b"])
	require.NotNil(t, wip.StartedAt)

	done, err := store.Get(ctx, PlanID(projectID, "feat-done"))
	require.NoError(t, err)
	require.Equal(t, PlanStatusCompleted, done.Status)
	require.NotNil(t, done.CompletedAt)
	require.Equal(t, "done", done.TaskProgress["t1"])

	arch, err := store.Get(ctx, PlanID(projectID, "feat-old"))
	require.NoError(t, err)
	require.Equal(t, PlanStatusArchived, arch.Status)
	require.NotNil(t, arch.ArchivedAt)

	// Source files untouched.
	for _, rel := range []string{
		"plans/pending/feat-pending.yaml",
		"plans/in_progress/feat-wip.yaml",
		"plans/completed/feat-done.yaml",
		"plans/archived/feat-old.yaml",
	} {
		_, err := os.Stat(filepath.Join(root, rel))
		require.NoError(t, err)
	}

	// Migration audit events recorded.
	events, err := store.ListEvents(ctx, wip.ID, LegacyImportExecutionID)
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, EventKindLegacyImported, events[0].Kind)
}

func TestImportLegacy_idempotentHashMatchAndConflict(t *testing.T) {
	svc, store, root, _ := newTestService(t)
	ctx := context.Background()
	projectID := "proj-dup"

	body := legacyYAML("dup-plan", "stable goal",
		"  - id: t1\n    prompt: work\n")
	writeLegacyPlan(t, root, "plans/pending/dup-plan.yaml", body)

	r1, err := svc.ImportLegacy(ctx, projectID, ImportOptions{})
	require.NoError(t, err)
	require.Equal(t, 1, len(r1.Imported))

	r2, err := svc.ImportLegacy(ctx, projectID, ImportOptions{})
	require.NoError(t, err)
	require.Equal(t, 0, len(r2.Imported))
	require.Equal(t, 1, len(r2.Skipped))
	require.Equal(t, ImportOutcomeSkipped, r2.Skipped[0].Outcome)
	require.Contains(t, r2.Skipped[0].Reason, "content hash match")

	// Mutate YAML definition → conflict; existing record unchanged.
	writeLegacyPlan(t, root, "plans/pending/dup-plan.yaml", legacyYAML("dup-plan", "CHANGED goal",
		"  - id: t1\n    prompt: work\n"))
	before, err := store.Get(ctx, PlanID(projectID, "dup-plan"))
	require.NoError(t, err)

	r3, err := svc.ImportLegacy(ctx, projectID, ImportOptions{})
	require.NoError(t, err)
	require.Equal(t, 1, len(r3.Conflicted))
	require.Equal(t, ImportOutcomeConflicted, r3.Conflicted[0].Outcome)
	require.NotEmpty(t, r3.Conflicted[0].ExistingHash)
	require.NotEqual(t, r3.Conflicted[0].ExistingHash, r3.Conflicted[0].ContentHash)

	after, err := store.Get(ctx, PlanID(projectID, "dup-plan"))
	require.NoError(t, err)
	require.Equal(t, before.Goal, after.Goal)
	require.Equal(t, before.ContentHash, after.ContentHash)
	require.Equal(t, before.Revision, after.Revision)
}

func TestImportLegacy_malformedAndMissingTaskIDs(t *testing.T) {
	svc, _, root, _ := newTestService(t)
	ctx := context.Background()
	projectID := "proj-bad"

	writeLegacyPlan(t, root, "plans/pending/broken.yaml", "name: broken\ngoal: [\n  not: yaml\n")
	writeLegacyPlan(t, root, "plans/pending/no-ids.yaml", "version: 1\nname: no-ids\ngoal: g\ntasks:\n  - prompt: missing id\n")
	writeLegacyPlan(t, root, "plans/pending/good.yaml", legacyYAML("good", "ok",
		"  - id: t1\n    prompt: fine\n"))

	report, err := svc.ImportLegacy(ctx, projectID, ImportOptions{})
	require.NoError(t, err)
	require.Equal(t, 1, len(report.Imported))
	require.Equal(t, 2, len(report.Errors))
	var reasons []string
	for _, e := range report.Errors {
		reasons = append(reasons, e.Reason)
	}
	require.Contains(t, reasons[0]+reasons[1], "malformed yaml")
	require.Contains(t, reasons[0]+reasons[1], "missing id")
}

func TestImportLegacy_reportOnlyLeavesStoreUntouched(t *testing.T) {
	svc, store, root, _ := newTestService(t)
	ctx := context.Background()
	projectID := "proj-report"

	writeLegacyPlan(t, root, "plans/pending/r1.yaml", legacyYAML("r1", "g",
		"  - id: t1\n    prompt: p\n"))

	report, err := svc.ImportLegacy(ctx, projectID, ImportOptions{ReportOnly: true})
	require.NoError(t, err)
	require.True(t, report.ReportOnly)
	require.Equal(t, 1, len(report.Imported))
	require.Equal(t, "would create", report.Imported[0].Reason)

	_, err = store.Get(ctx, PlanID(projectID, "r1"))
	require.ErrorIs(t, err, ErrNotFound)

	events, err := store.ListEvents(ctx, PlanID(projectID, "r1"), LegacyImportExecutionID)
	require.NoError(t, err)
	require.Empty(t, events)
}

func TestImportLegacy_preservesInProgressExecutionLinks(t *testing.T) {
	svc, store, root, _ := newTestService(t)
	ctx := context.Background()
	projectID := "proj-exec"
	name := "linked-plan"
	id := PlanID(projectID, name)

	// Prior scan-only record with execution links but empty definition.
	started := time.Now().UTC().Add(-time.Hour)
	require.NoError(t, store.Create(ctx, &Plan{
		ID:             id,
		ProjectID:      projectID,
		Name:           name,
		FilePath:       "plans/in_progress/linked-plan.yaml",
		Status:         PlanStatusInProgress,
		AutopilotRunID: "ap-run-keep",
		PipelineID:     "",
		StartedAt:      &started,
		TaskProgress:   map[string]string{"t1": "in_progress"},
	}))

	writeLegacyPlan(t, root, "plans/in_progress/linked-plan.yaml", legacyYAML(name, "from yaml",
		"  - id: t1\n    prompt: keep going\n    after: []\n  - id: t2\n    prompt: next\n    after: [t1]\n"))

	report, err := svc.ImportLegacy(ctx, projectID, ImportOptions{})
	require.NoError(t, err)
	require.Equal(t, 1, len(report.Imported))
	require.True(t, report.Imported[0].Reconciled)

	got, err := store.Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "from yaml", got.Goal)
	require.Equal(t, "ap-run-keep", got.AutopilotRunID)
	require.Equal(t, PlanStatusInProgress, got.Status)
	require.NotNil(t, got.StartedAt)
	require.Equal(t, started.Unix(), got.StartedAt.Unix())
	require.Equal(t, "in_progress", got.TaskProgress["t1"])
	require.Equal(t, "pending", got.TaskProgress["t2"])
	require.Equal(t, []string{"t1"}, got.Tasks[1].After)
}

func TestImportLegacy_twoConsecutiveDaemonStartsDoNotRescan(t *testing.T) {
	// Phase 5 cutover: ImportLegacy is operator-invoked only. Two consecutive
	// "daemon starts" are modelled as reopening the ScrivaDB store and listing
	// without calling ImportLegacy — records must be unchanged. A later
	// explicit ImportLegacy is a pure skip when hashes match.
	svc, store, root, _ := newTestService(t)
	ctx := context.Background()
	projectID := "proj-restart"

	writeLegacyPlan(t, root, "plans/pending/restart.yaml", legacyYAML("restart", "g",
		"  - id: t1\n    prompt: p\n"))

	_, err := svc.ImportLegacy(ctx, projectID, ImportOptions{})
	require.NoError(t, err)
	before, err := store.Get(ctx, PlanID(projectID, "restart"))
	require.NoError(t, err)

	list1, err := store.ListByProject(ctx, projectID)
	require.NoError(t, err)
	require.Len(t, list1, 1)

	list2, err := store.ListByProject(ctx, projectID)
	require.NoError(t, err)
	require.Len(t, list2, 1)
	require.Equal(t, before.ContentHash, list2[0].ContentHash)
	require.Equal(t, before.Revision, list2[0].Revision)

	r, err := svc.ImportLegacy(ctx, projectID, ImportOptions{})
	require.NoError(t, err)
	require.Equal(t, 1, len(r.Skipped))
	after, err := store.Get(ctx, before.ID)
	require.NoError(t, err)
	require.Equal(t, before.UpdatedAt.UnixNano(), after.UpdatedAt.UnixNano())
}

func TestImportLegacy_noPlansDir(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	ctx := context.Background()
	report, err := svc.ImportLegacy(ctx, "proj-empty", ImportOptions{})
	require.NoError(t, err)
	imported, skipped, conflicted, errored := report.Counts()
	require.Zero(t, imported+skipped+conflicted+errored)
}

func TestImportLegacy_envelopeLifecycleOverridesPath(t *testing.T) {
	svc, store, root, _ := newTestService(t)
	ctx := context.Background()
	projectID := "proj-env"
	name := "env-plan"
	body := "schema_version: 1\nplan_id: " + PlanID(projectID, name) + "\n" +
		"revision: 1\ncontent_hash: sha256:dead\nexported_at: 2026-09-30T12:00:00Z\n" +
		"lifecycle: completed\nexecution_summary_ref: null\n" +
		legacyYAML(name, "g", "  - id: t1\n    prompt: p\n")
	// File sits under pending/, but envelope says completed.
	writeLegacyPlan(t, root, "plans/pending/env-plan.yaml", body)

	report, err := svc.ImportLegacy(ctx, projectID, ImportOptions{})
	require.NoError(t, err)
	require.Equal(t, 1, len(report.Imported))
	require.Equal(t, PlanStatusCompleted, report.Imported[0].Status)

	got, err := store.Get(ctx, PlanID(projectID, name))
	require.NoError(t, err)
	require.Equal(t, PlanStatusCompleted, got.Status)
	require.NotNil(t, got.CompletedAt)
}
