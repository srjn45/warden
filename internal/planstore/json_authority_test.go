package planstore

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// JSON repository replicas (#585) must never become execution source-of-truth.
// Scan/import-legacy only discover *.yaml/*.yml; PlanService Get/List/Update
// read ScrivaDB only. These tests lock that boundary so a future format walker
// cannot quietly treat JSON exports as legacy authority.

func jsonReplicaBody(name, goal, planID string) string {
	return "{\n" +
		`  "warden_plan_export": "replica only — not authoritative",` + "\n" +
		`  "schema_version": 1,` + "\n" +
		`  "plan_id": "` + planID + `",` + "\n" +
		`  "revision": 99,` + "\n" +
		`  "content_hash": "sha256:deadbeef",` + "\n" +
		`  "exported_at": "2026-09-30T12:00:00Z",` + "\n" +
		`  "lifecycle": "in_progress",` + "\n" +
		`  "execution_summary_ref": null,` + "\n" +
		`  "version": 1,` + "\n" +
		`  "name": "` + name + `",` + "\n" +
		`  "goal": "` + goal + `",` + "\n" +
		`  "tasks": [{"id": "evil", "prompt": "should never become SoT"}]` + "\n" +
		"}\n"
}

func writeJSONReplica(t *testing.T, root, rel, body string) {
	t.Helper()
	abs := filepath.Join(root, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o755))
	require.NoError(t, os.WriteFile(abs, []byte(body), 0o644))
}

func TestScanProject_ignoresJSONReplicas(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	root := t.TempDir()

	writeJSONReplica(t, root, "plans/pending/json-only.json",
		jsonReplicaBody("json-only", "hijack via scan", "plan-jsononly"))
	writeJSONReplica(t, root, "plans/in_progress/wip.json",
		jsonReplicaBody("wip", "hijack via dir", "plan-wipjson"))
	writeJSONReplica(t, root, "plans/flat-export.json",
		jsonReplicaBody("flat-export", "hijack flat", "plan-flatjson"))
	// A real YAML sibling still creates a stub — proves the walker ran.
	writePlanFile(t, root, "plans/pending/yaml-sibling.yaml", "yaml-sibling")

	res, err := ScanProject(ctx, s, "proj-json-scan", root)
	require.NoError(t, err)
	require.Equal(t, 1, res.Upserted)
	require.Equal(t, 0, res.SkippedCanonical)

	list, err := s.ListByProject(ctx, "proj-json-scan")
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, "yaml-sibling", list[0].Name)

	_, err = s.Get(ctx, PlanID("proj-json-scan", "json-only"))
	require.Error(t, err, "JSON replica must not create a plan via scan")
	_, err = s.Get(ctx, PlanID("proj-json-scan", "wip"))
	require.Error(t, err)
	_, err = s.Get(ctx, PlanID("proj-json-scan", "flat-export"))
	require.Error(t, err)
}

func TestImportLegacy_ignoresJSONReplicas(t *testing.T) {
	svc, store, root, _ := newTestService(t)
	ctx := context.Background()
	projectID := "proj-json-import"

	writeJSONReplica(t, root, "plans/pending/json-only.json",
		jsonReplicaBody("json-only", "hijack via import-legacy", "plan-jsononly"))
	writeJSONReplica(t, root, "plans/completed/done.json",
		jsonReplicaBody("done", "completed json", "plan-donejson"))
	writeLegacyPlan(t, root, "plans/pending/yaml-ok.yaml", legacyYAML("yaml-ok", "from yaml",
		"  - id: t1\n    prompt: keep me\n"))

	report, err := svc.ImportLegacy(ctx, projectID, ImportOptions{})
	require.NoError(t, err)
	imported, skipped, conflicted, errored := report.Counts()
	require.Equal(t, 1, imported)
	require.Equal(t, 0, skipped)
	require.Equal(t, 0, conflicted)
	require.Equal(t, 0, errored)
	require.Equal(t, "plans/pending/yaml-ok.yaml", report.Imported[0].FilePath)

	list, err := store.ListByProject(ctx, projectID)
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, "yaml-ok", list[0].Name)
	require.Equal(t, "from yaml", list[0].Goal)

	_, err = store.Get(ctx, PlanID(projectID, "json-only"))
	require.Error(t, err, "JSON replica must never be import-legacy authority")
	_, err = store.Get(ctx, PlanID(projectID, "done"))
	require.Error(t, err)
}

func TestPlanService_EditedJSONDoesNotMutateCanonical(t *testing.T) {
	svc, store, root, _ := newTestService(t)
	ctx := context.Background()

	p, err := svc.Create(ctx, "proj-1", sampleCreate("CanonicalJSON"))
	require.NoError(t, err)
	before, err := store.Get(ctx, p.ID)
	require.NoError(t, err)

	rel := filepath.Join("plans", "pending", "canonical-json.json")
	writeJSONReplica(t, root, rel, jsonReplicaBody("CanonicalJSON", "hijacked from json disk", p.ID))
	require.NoError(t, store.Update(ctx, p.ID, func(pl *Plan) error {
		pl.FilePath = rel // last-export metadata only
		return nil
	}))

	got, err := svc.Get(ctx, p.ID)
	require.NoError(t, err)
	require.Equal(t, before.Goal, got.Goal)
	require.Equal(t, before.ContentHash, got.ContentHash)
	require.Equal(t, before.Revision, got.Revision)
	require.Len(t, got.Tasks, 2)
	require.Equal(t, "t1", got.Tasks[0].ID)

	listed, err := svc.List(ctx, "proj-1", PlanStatusPending)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.Equal(t, before.Goal, listed[0].Goal)

	// Mutate the JSON replica after a canonical update — still inert.
	goal := "canonical json-safe update"
	got, err = svc.Update(ctx, p.ID, UpdateRequest{Goal: &goal})
	require.NoError(t, err)
	require.Equal(t, "canonical json-safe update", got.Goal)
	writeJSONReplica(t, root, rel, jsonReplicaBody("CanonicalJSON", "disk again after update", p.ID))

	got, err = svc.Get(ctx, p.ID)
	require.NoError(t, err)
	require.Equal(t, "canonical json-safe update", got.Goal)
	require.Equal(t, "t1", got.Tasks[0].ID)
	require.NotEqual(t, "hijacked from json disk", got.Goal)
}

func TestScanProject_JSONReplicaDoesNotReseedCanonical(t *testing.T) {
	// Even if an operator drops a disagreeing JSON export next to a canonical
	// Plan (and points FilePath at it), scan must not reseed definition/status.
	svc, store, root, _ := newTestService(t)
	ctx := context.Background()
	projectID := "proj-json-reseed"

	p, err := svc.Create(ctx, projectID, sampleCreate("ship-json"))
	require.NoError(t, err)
	before, err := store.Get(ctx, p.ID)
	require.NoError(t, err)
	require.Equal(t, PlanStatusPending, before.Status)

	writeJSONReplica(t, root, "plans/in_progress/ship-json.json",
		jsonReplicaBody("ship-json", "json wants in_progress + new goal", p.ID))

	scanRes, err := ScanProject(ctx, store, projectID, root)
	require.NoError(t, err)
	require.Equal(t, 0, scanRes.Upserted)
	require.Equal(t, 0, scanRes.SkippedCanonical, "JSON files are not scan candidates")

	after, err := store.Get(ctx, p.ID)
	require.NoError(t, err)
	require.Equal(t, PlanStatusPending, after.Status)
	require.Equal(t, before.Goal, after.Goal)
	require.Equal(t, before.ContentHash, after.ContentHash)
	require.Equal(t, before.Revision, after.Revision)
}
