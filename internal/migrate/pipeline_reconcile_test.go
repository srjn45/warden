package migrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/srjn45/scriva"
	"github.com/srjn45/scriva/engine"
	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/schema"
	"github.com/srjn45/warden/internal/storage"
	"github.com/srjn45/warden/internal/store"
	"github.com/srjn45/warden/internal/wardendb"
)

// Synthetic fixtures only: every id, path and prompt below is invented.

func reconcilePipe(id string, status pipeline.Status) pipeline.Pipeline {
	return pipeline.Pipeline{
		ID: id, Name: id, Status: status, ProjectID: "/repo",
		Jobs: []pipeline.Job{
			{ID: "alpha", Prompt: "a", Worktree: "fresh", Status: pipeline.JobDone},
			{ID: "beta", Prompt: "b", Worktree: "none", DependsOn: []string{"alpha"}, Status: pipeline.JobDone},
		},
	}
}

// writeLegacyPipelineFile writes the schema-1 pipelines/<id>.json copy.
func writeLegacyPipelineFile(t *testing.T, d string, p pipeline.Pipeline) {
	t.Helper()
	raw, err := json.Marshal(p)
	require.NoError(t, err)
	writeFile(t, d, "pipelines/"+p.ID+".json", string(raw))
}

// importThenDelete leaves exactly what a v10 install leaves behind for a
// pipeline that was imported and later deleted: a legacy file plus a
// pipelines-db delete tombstone.
func importThenDelete(t *testing.T, d string, p pipeline.Pipeline) {
	t.Helper()
	writeLegacyPipelineFile(t, d, p)
	writeScrivaRecord(t, filepath.Join(d, "pipelines-db"), "pipelines", p.ID, p)
	db, err := scriva.Open(filepath.Join(d, "pipelines-db"), scriva.WithSyncMode(engine.SyncModeAlways))
	require.NoError(t, err)
	defer db.Close()
	col, err := db.Collection("pipelines")
	require.NoError(t, err)
	require.NoError(t, col.DeleteByKey(p.ID))
}

func addAgentRow(t *testing.T, d, coll, id string, status store.Status, pipeID, jobID string) {
	t.Helper()
	writeScrivaRecord(t, filepath.Join(d, "agents-db"), coll, id, agentstore.Agent{
		ID: id, Name: id, Type: store.TypeDevelopment, Status: status, TmuxSession: "tmux-" + id,
		ProjectID: "/repo", PipelineID: pipeID, JobID: jobID, CreatedAt: t0, UpdatedAt: t0,
	})
}

func addPlanRow(t *testing.T, d, id, status, pipeID string) {
	t.Helper()
	body := map[string]any{"id": id, "project_id": "/repo", "status": status}
	if pipeID != "" {
		body["pipeline_id"] = pipeID
	}
	writeScrivaRecord(t, filepath.Join(d, "plans", "plans-db"), "plans", id, body)
}

func exportTo(t *testing.T, d string) (*StagingExportReport, string, error) {
	t.Helper()
	target := filepath.Join(t.TempDir(), "staging")
	rep, err := ExportSchema2Staging(context.Background(), d, StagingExportOptions{TargetDir: target, Now: t0})
	return rep, target, err
}

func findingsOf(fs []Finding, class FindingClass) []Finding {
	var out []Finding
	for _, f := range fs {
		if f.Class == string(class) {
			out = append(out, f)
		}
	}
	return out
}

func requireClass(t *testing.T, fs []Finding, class FindingClass, sev Severity, contains string) Finding {
	t.Helper()
	for _, f := range findingsOf(fs, class) {
		if f.Severity == sev && strings.Contains(f.Message, contains) {
			return f
		}
	}
	t.Fatalf("no %s %s finding containing %q in %+v", sev, class, contains, fs)
	return Finding{}
}

func stagedRow(t *testing.T, target, col, key string) map[string]any {
	t.Helper()
	db, err := scriva.Open(target, scriva.WithSyncMode(engine.SyncModeNone))
	require.NoError(t, err)
	defer db.Close()
	return stagingRecord(t, db, col, key)
}

func TestReconcilePipelines_IdenticalLegacyCopyIsDeduplicated(t *testing.T) {
	d := stagingLegacyDir(t)
	writeLegacyPipelineFile(t, d, pipeline.Pipeline{
		ID: "pipe-1", Name: "pipe-1", Status: pipeline.StatusRunning, ProjectID: "/repo",
		Jobs: []pipeline.Job{
			{ID: "build", Status: pipeline.JobRunning, AgentID: "agent-1"},
			{ID: "test", Status: pipeline.JobPending, DependsOn: []string{"build"}},
		},
	})
	before := snapshotTree(t, d)
	rep, _, err := exportTo(t, d)
	require.NoError(t, err, "%+v", rep.Findings)
	require.Equal(t, 1, rep.Records["pipelines"], "the duplicate must not add a second pipeline")
	require.Equal(t, 2, rep.Records["pipeline_jobs"])
	requireClass(t, rep.Findings, ClassPipelineSuperseded, SeverityAuto, "identical to their pipelines-db record")
	require.Equal(t, before, snapshotTree(t, d))
}

func TestReconcilePipelines_DivergentLegacyCopyIsAConflictNamingFieldsOnly(t *testing.T) {
	d := stagingLegacyDir(t)
	writeLegacyPipelineFile(t, d, pipeline.Pipeline{
		ID: "pipe-1", Name: "SECRET-RENAMED", Status: pipeline.StatusDone, ProjectID: "/repo",
		Jobs: []pipeline.Job{{ID: "build", Status: pipeline.JobDone, AgentID: "agent-1"}, {ID: "test", Status: pipeline.JobDone, DependsOn: []string{"build"}}},
	})
	rep, _, err := exportTo(t, d)
	require.ErrorIs(t, err, ErrStagingExportRefused)
	require.True(t, rep.Blocked)
	f := requireClass(t, rep.Findings, ClassPipelineLegacyConflict, SeverityBlocking, "pipe-1")
	require.Contains(t, f.Message, "name")
	require.Contains(t, f.Message, "status")
	require.NotContains(t, f.Message, "SECRET-RENAMED", "diagnostics name fields, never copy values")
	require.NotEmpty(t, f.Details, "a conflict must say how to resolve it")
}

func TestReconcilePipelines_DeletedPipelineKeepsHistoricalProvenanceWithoutResurrection(t *testing.T) {
	d := stagingLegacyDir(t)
	importThenDelete(t, d, reconcilePipe("plan-gone", pipeline.StatusDone))
	addAgentRow(t, d, "closed", "agent-hist", store.StatusIdle, "plan-gone", "alpha")
	addAgentRow(t, d, "agents", "agent-finished", store.StatusDone, "plan-gone", "beta")
	addPlanRow(t, d, "plan-hist", "archived", "plan-gone")
	before := snapshotTree(t, d)

	rep, target, err := exportTo(t, d)
	require.NoError(t, err, "%+v", rep.Findings)
	require.Equal(t, 1, rep.Records["pipelines"], "a deleted pipeline must not be resurrected from its legacy copy")
	require.Equal(t, 2, rep.Records["pipeline_jobs"])

	for _, tc := range []struct{ col, key, job string }{
		{"archived_agents", "agent-hist", "alpha"}, {"agents", "agent-finished", "beta"}, {"plans", "plan-hist", ""},
	} {
		row := stagedRow(t, target, tc.col, tc.key)
		require.NotContains(t, row, "pipeline_id", "%s/%s must not keep a dangling parent", tc.col, tc.key)
		require.NotContains(t, row, "job_id")
		ref, ok := row["historical_pipeline"].(map[string]any)
		require.True(t, ok, "%s/%s needs provenance: %v", tc.col, tc.key, row)
		require.Equal(t, "plan-gone", ref["id"])
		require.Equal(t, wardendb.HistoricalDeleted, ref["reason"])
		require.Equal(t, Migration0002ID, ref["recorded_by"])
		require.Contains(t, ref["evidence"], "delete tombstone")
		if tc.job != "" {
			require.Equal(t, tc.job, ref["job_id"])
		}
	}
	note := requireClass(t, rep.Findings, ClassPipelineHistoricalRef, SeverityRepairable, "plan-gone")
	require.Contains(t, note.Message, "3 historical record(s)")
	requireClass(t, rep.Findings, ClassPipelineSuperseded, SeverityAuto, "stale copies of pipelines deleted after the import")

	eq, err := VerifySchema2Equivalence(context.Background(), d, target)
	require.NoError(t, err, "%+v", eq.Findings)
	require.True(t, eq.Passed)
	require.Equal(t, before, snapshotTree(t, d), "no source write")
}

func TestReconcilePipelines_ProvenanceSurvivesRuntimeStoreRoundTrip(t *testing.T) {
	ref := &wardendb.HistoricalRef{Kind: wardendb.HistoricalKindPipeline, ID: "plan-gone", JobID: "alpha", Reason: wardendb.HistoricalDeleted, RecordedBy: Migration0002ID}
	raw, err := json.Marshal(agentstore.Agent{ID: "a", HistoricalPipeline: ref})
	require.NoError(t, err)
	var back agentstore.Agent
	require.NoError(t, json.Unmarshal(raw, &back))
	require.Equal(t, ref, back.HistoricalPipeline, "the typed runtime agent must not drop the provenance on a rewrite")
}

func TestReconcilePipelines_UnprovenDeletionBlocks(t *testing.T) {
	d := stagingLegacyDir(t)
	// A legacy file with NO pipelines-db record and NO tombstone: either never
	// imported or deleted after a compaction. That is not provable.
	writeLegacyPipelineFile(t, d, reconcilePipe("plan-mystery", pipeline.StatusDone))
	addAgentRow(t, d, "closed", "agent-hist", store.StatusDone, "plan-mystery", "alpha")

	rep, target, err := exportTo(t, d)
	require.ErrorIs(t, err, ErrStagingExportRefused)
	f := requireClass(t, rep.Findings, ClassPipelineUnprovenDeletion, SeverityBlocking, "plan-mystery")
	require.Contains(t, f.Details, "move the legacy file aside")
	require.Empty(t, findingsOf(rep.Findings, ClassPipelineHistoricalRef), "a possibly-live pipeline's referrers must not be rewritten")
	_, statErr := os.Stat(target)
	require.True(t, errors.Is(statErr, os.ErrNotExist), "a refused export writes no staging")
}

func TestReconcilePipelines_ActiveOwnerOfDeletedPipelineBlocks(t *testing.T) {
	for _, st := range []store.Status{store.StatusWorking, store.StatusIdle, store.StatusWaitingForInput, store.StatusSpawning, store.StatusRateLimited} {
		t.Run(string(st), func(t *testing.T) {
			d := stagingLegacyDir(t)
			importThenDelete(t, d, reconcilePipe("plan-gone", pipeline.StatusDone))
			addAgentRow(t, d, "agents", "agent-live", st, "plan-gone", "alpha")
			addAgentRow(t, d, "closed", "agent-hist", store.StatusDone, "plan-gone", "beta")

			rep, _, err := exportTo(t, d)
			require.ErrorIs(t, err, ErrStagingExportRefused)
			f := requireClass(t, rep.Findings, ClassPipelineActiveRef, SeverityBlocking, "agent-live")
			require.Contains(t, f.Message, string(st))
			require.Contains(t, f.Details, "never nulls the reference")
			require.Empty(t, findingsOf(rep.Findings, ClassPipelineHistoricalRef), "nothing is rewritten when the pipeline's owner set is blocked")
		})
	}
}

func TestReconcilePipelines_FinishedAndOrphanedAgentsAreHistorical(t *testing.T) {
	for _, st := range []store.Status{store.StatusDone, store.StatusErrored, store.StatusOrphaned} {
		t.Run(string(st), func(t *testing.T) {
			d := stagingLegacyDir(t)
			importThenDelete(t, d, reconcilePipe("plan-gone", pipeline.StatusDone))
			addAgentRow(t, d, "agents", "agent-x", st, "plan-gone", "alpha")
			rep, target, err := exportTo(t, d)
			require.NoError(t, err, "%+v", rep.Findings)
			require.Contains(t, stagedRow(t, target, "agents", "agent-x"), "historical_pipeline")
		})
	}
}

func TestReconcilePipelines_UnrecordedHistoricalReferenceIsPreservedAsSuch(t *testing.T) {
	d := stagingLegacyDir(t)
	// No legacy file, no tombstone: the pipeline never existed in any source.
	addAgentRow(t, d, "closed", "agent-hist", store.StatusDone, "plan-never", "alpha")
	rep, target, err := exportTo(t, d)
	require.NoError(t, err, "%+v", rep.Findings)
	ref := stagedRow(t, target, "archived_agents", "agent-hist")["historical_pipeline"].(map[string]any)
	require.Equal(t, wardendb.HistoricalUnrecorded, ref["reason"])
	require.Contains(t, ref["evidence"], "no pipeline record")
}

func TestReconcilePipelines_ActivePlanOfDeletedPipelineBlocks(t *testing.T) {
	for _, st := range []string{"pending", "in_progress"} {
		d := stagingLegacyDir(t)
		importThenDelete(t, d, reconcilePipe("plan-gone", pipeline.StatusDone))
		addPlanRow(t, d, "plan-live", st, "plan-gone")
		rep, _, err := exportTo(t, d)
		require.ErrorIs(t, err, ErrStagingExportRefused, st)
		requireClass(t, rep.Findings, ClassPipelineActiveRef, SeverityBlocking, "plan plan-live")
	}
	d := stagingLegacyDir(t)
	addPlanRow(t, d, "plan-dangling", "pending", "plan-never")
	rep, _, err := exportTo(t, d)
	require.ErrorIs(t, err, ErrStagingExportRefused, "a pipeline-less active plan reference is a blocker too")
	requireClass(t, rep.Findings, ClassPipelineActiveRef, SeverityBlocking, "plan-dangling")
}

func TestReconcilePipelines_DanglingJobLinksStayBlocking(t *testing.T) {
	d := stagingLegacyDir(t)
	// pipe-1 is live: a binding to a job it does not have is not "history".
	addAgentRow(t, d, "closed", "agent-hist", store.StatusDone, "pipe-1", "ghost-job")
	rep, _, err := exportTo(t, d)
	require.ErrorIs(t, err, ErrStagingExportRefused)
	requireBlocking(t, rep.Findings, "agents", "job binding pipe-1:ghost-job does not exist")

	d = stagingLegacyDir(t)
	writeScrivaRecord(t, filepath.Join(d, "pipelines-db"), "pipelines", "pipe-2", pipeline.Pipeline{
		ID: "pipe-2", Name: "pipe-2", Status: pipeline.StatusDone, ProjectID: "/repo",
		Jobs: []pipeline.Job{{ID: "j", AgentID: "agent-missing", Status: pipeline.JobDone}},
	})
	rep, _, err = exportTo(t, d)
	require.ErrorIs(t, err, ErrStagingExportRefused)
	requireBlocking(t, rep.Findings, "pipeline_jobs", "agent_id agent-missing does not exist")
}

func TestReconcilePipelines_RecreatedPipelineIsNotATombstone(t *testing.T) {
	d := stagingLegacyDir(t)
	p := reconcilePipe("plan-again", pipeline.StatusDone)
	importThenDelete(t, d, p)
	writeScrivaRecord(t, filepath.Join(d, "pipelines-db"), "pipelines", p.ID, p) // same key, new record
	tombs, err := readPipelineTombstones(d)
	require.NoError(t, err)
	require.NotContains(t, tombs, "plan-again", "a re-inserted key is live again")

	rep, _, err := exportTo(t, d)
	require.NoError(t, err, "%+v", rep.Findings)
	requireClass(t, rep.Findings, ClassPipelineSuperseded, SeverityAuto, "identical")
	require.Equal(t, 2, rep.Records["pipelines"])
}

func TestReconcilePipelines_TombstoneReaderIgnoresUnattributableDeletes(t *testing.T) {
	d := t.TempDir()
	colDir := filepath.Join(d, "pipelines-db", "pipelines")
	require.NoError(t, os.MkdirAll(colDir, 0o700))
	seg := strings.Join([]string{
		`{"id":1,"op":"insert","ts":"2026-01-01T00:00:00Z","data":{"_key":"p1","id":"p1"}}`,
		`{"id":1,"op":"delete","ts":"2026-01-02T00:00:00Z"}`,
		`{"id":99,"op":"delete","ts":"2026-01-02T00:00:00Z"}`, // its insert was compacted away
		`not json at all`,
		`{"id":2,"op":"insert","ts":"2026-01-03T00:00:00Z","data":{"_key":"p2","id":"p2"}}`,
	}, "\n") + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(colDir, "seg_000001.ndjson"), []byte(seg), 0o600))
	tombs, err := readPipelineTombstones(d)
	require.NoError(t, err)
	require.Equal(t, map[string]pipelineTombstone{"p1": {RecordID: 1, DeletedAt: "2026-01-02T00:00:00Z", Segment: "seg_000001.ndjson"}}, tombs)

	none, err := readPipelineTombstones(t.TempDir())
	require.NoError(t, err)
	require.Empty(t, none, "no pipelines-db means no proof, not an error")
}

func TestReconcilePipelines_LegacyDirectoryHazardsAreTypedAndBounded(t *testing.T) {
	d := stagingLegacyDir(t)
	writeFile(t, d, "pipelines/broken.json", `{"id": "x", "jobs": [`+strings.Repeat("A", 4000))
	writeFile(t, d, "pipelines/noid.json", `{"name":"x"}`)
	writeFile(t, d, "pipelines/other.json", `{"id":"someone-else","name":"x"}`)
	writeFile(t, d, "pipelines/unsafe.json", `{"id":"../unsafe","name":"x"}`)
	writeFile(t, d, "pipelines/notes.txt", `hello`)
	writeFile(t, d, "pipelines/.tmp-123", `partial`)
	writeFile(t, d, "pipelines/sub/inner.json", `{}`)
	require.NoError(t, os.Symlink("/etc/hostname", filepath.Join(d, "pipelines", "link.json")))
	before := snapshotTree(t, d)

	rep, _, err := exportTo(t, d)
	require.ErrorIs(t, err, ErrStagingExportRefused)
	broken := requireClass(t, rep.Findings, ClassCorruption, SeverityBlocking, "pipelines/broken.json: malformed pipeline JSON")
	require.Less(t, len(broken.Message), 400, "a decode error must be bounded and must not echo the file")
	require.NotContains(t, broken.Message, "AAAAAAAAAA")
	requireClass(t, rep.Findings, ClassCorruption, SeverityBlocking, "pipelines/noid.json: pipeline has no id")
	requireClass(t, rep.Findings, ClassCorruption, SeverityBlocking, "pipelines/unsafe.json: unsafe pipeline id")
	requireClass(t, rep.Findings, ClassAmbiguity, SeverityBlocking, "pipelines/other.json: file name does not match")
	requireClass(t, rep.Findings, ClassUnsupportedLayout, SeverityBlocking, "pipelines/notes.txt: file has no safe staging mapping")
	requireClass(t, rep.Findings, ClassUnsupportedLayout, SeverityBlocking, "pipelines/sub: unexpected directory")
	requireClass(t, rep.Findings, ClassUnsafeArtifact, SeverityBlocking, "pipelines/link.json: symlink")
	requireClass(t, rep.Findings, ClassUnsupportedLayout, SeverityRepairable, "pipelines/.tmp-123: leftover temp file ignored")
	require.Equal(t, before, snapshotTree(t, d))
}

func TestReconcilePipelines_DuplicateLegacyIdentityBlocks(t *testing.T) {
	d := stagingLegacyDir(t)
	// Two files cannot both be <id>.json, but a case-folding or hand-copied
	// directory can still produce a second declaration of the same id.
	writeFile(t, d, "pipelines/plan-a.json", `{"id":"plan-a","name":"plan-a","status":"done","jobs":[]}`)
	writeFile(t, d, "pipelines/plan-a.JSON.json", `{"id":"plan-a","name":"plan-a","status":"done","jobs":[]}`)
	rep, _, err := exportTo(t, d)
	require.ErrorIs(t, err, ErrStagingExportRefused)
	// the second file fails the name/id check; either way a duplicate never merges
	require.NotEmpty(t, findingsOf(rep.Findings, ClassAmbiguity))
}

func TestReconcilePipelines_DiagnosticsAreBounded(t *testing.T) {
	d := stagingLegacyDir(t)
	const n = 60
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("plan-%03d", i)
		importThenDelete(t, d, reconcilePipe(id, pipeline.StatusDone))
		addAgentRow(t, d, "agents", "agent-live-"+id, store.StatusWorking, id, "alpha")
	}
	rep, _, err := exportTo(t, d)
	require.ErrorIs(t, err, ErrStagingExportRefused)
	require.Len(t, findingsOf(rep.Findings, ClassPipelineActiveRef), maxPipelineFindings)
	requireBlocking(t, rep.Findings, "pipelines", fmt.Sprintf("only the first %d are listed", maxPipelineFindings))

	var re *RefusalError
	require.ErrorAs(t, err, &re)
	require.Equal(t, rep.Findings, re.Findings)
	msg := err.Error()
	require.Contains(t, msg, "[pipeline_active_ref]")
	require.Contains(t, msg, fmt.Sprintf("and %d more", countBlocking(rep.Findings)-maxRefusalListed))
	require.Less(t, strings.Count(msg, "[pipeline_active_ref]"), maxRefusalListed+1)
}

func TestReconcilePipelines_RepeatedExportIsStableAndSourceUntouched(t *testing.T) {
	d := stagingLegacyDir(t)
	importThenDelete(t, d, reconcilePipe("plan-gone", pipeline.StatusDone))
	addAgentRow(t, d, "closed", "agent-hist", store.StatusDone, "plan-gone", "alpha")
	before := snapshotTree(t, d)
	target := filepath.Join(t.TempDir(), "staging")

	first, err := ExportSchema2Staging(context.Background(), d, StagingExportOptions{TargetDir: target, Now: t0})
	require.NoError(t, err)
	second, err := ExportSchema2Staging(context.Background(), d, StagingExportOptions{TargetDir: target, Now: t0})
	require.NoError(t, err)
	require.Equal(t, first.Records, second.Records)
	require.Equal(t, first.Checksums, second.Checksums)
	require.Equal(t, first.Findings, second.Findings, "findings are deterministic")
	require.Equal(t, before, snapshotTree(t, d))
}

func TestReconcilePipelines_CancelledExportWritesNothing(t *testing.T) {
	d := stagingLegacyDir(t)
	importThenDelete(t, d, reconcilePipe("plan-gone", pipeline.StatusDone))
	before := snapshotTree(t, d)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	target := filepath.Join(t.TempDir(), "staging")
	_, err := ExportSchema2Staging(ctx, d, StagingExportOptions{TargetDir: target, Now: t0})
	require.ErrorIs(t, err, context.Canceled)
	_, statErr := os.Stat(target)
	require.True(t, errors.Is(statErr, os.ErrNotExist))
	require.Equal(t, before, snapshotTree(t, d))
}

func TestPreflightStagingMapping_ReportsBlockersReadOnly(t *testing.T) {
	d := stagingLegacyDir(t)
	importThenDelete(t, d, reconcilePipe("plan-gone", pipeline.StatusDone))
	addAgentRow(t, d, "agents", "agent-live", store.StatusWorking, "plan-gone", "alpha")
	before := snapshotTree(t, d)
	tmpBefore, _ := filepath.Glob(filepath.Join(os.TempDir(), "warden-mapping-preflight-*"))

	fs, err := PreflightStagingMapping(context.Background(), d)
	require.NoError(t, err)
	requireClass(t, fs, ClassPipelineActiveRef, SeverityBlocking, "agent-live")
	for _, f := range fs {
		require.NotContains(t, f.Message+f.Details, os.TempDir(), "the private preview path must not leak")
	}
	require.Equal(t, before, snapshotTree(t, d), "the preview must not write the data dir")
	tmpAfter, _ := filepath.Glob(filepath.Join(os.TempDir(), "warden-mapping-preflight-*"))
	require.Len(t, tmpAfter, len(tmpBefore), "the preview copy is removed")

	// A healthy dir previews clean of blockers.
	clean := stagingLegacyDir(t)
	fs, err = PreflightStagingMapping(context.Background(), clean)
	require.NoError(t, err)
	require.Zero(t, countBlocking(fs), "%+v", fs)
}

func TestPreflightStagingMapping_ReachesTheCutoverCheckBeforeAnyStop(t *testing.T) {
	d := t.TempDir()
	seedRealisticLegacy(t, d)
	// A pipeline deleted after import, still owned by a live agent.
	s, err := storage.OpenLegacy(d)
	require.NoError(t, err)
	gone := reconcilePipe("plan-gone", pipeline.StatusDone)
	require.NoError(t, s.Pipelines.Create(&gone))
	writeLegacyPipelineFile(t, d, gone)
	require.NoError(t, s.Pipelines.Delete("plan-gone"))
	require.NoError(t, s.Close())
	addAgentRow(t, d, "agents", "agent-owner", store.StatusWorking, "plan-gone", "alpha")
	before := snapshotTree(t, d)

	findings := checkSchema2Cutover(Env{DataDir: d, Context: context.Background()})
	requireClass(t, findings, ClassPipelineActiveRef, SeverityBlocking, "agent-owner")
	require.Equal(t, before, snapshotTree(t, d))
}

func TestCutover_RefusalSurfacesBlockersAndRollsBackCleanly(t *testing.T) {
	d := t.TempDir()
	seedRealisticLegacy(t, d)
	s, err := storage.OpenLegacy(d)
	require.NoError(t, err)
	gone := reconcilePipe("plan-gone", pipeline.StatusDone)
	require.NoError(t, s.Pipelines.Create(&gone))
	writeLegacyPipelineFile(t, d, gone)
	require.NoError(t, s.Pipelines.Delete("plan-gone"))
	require.NoError(t, s.Close())
	addAgentRow(t, d, "agents", "agent-owner", store.StatusWorking, "plan-gone", "alpha")
	before := snapshotTree(t, d)

	// Bypass the pre-stop check to prove the post-stop refusal itself explains why.
	run := Migration0002.Run
	err = run(Env{DataDir: d, BinaryVersion: "11.2.0", Context: context.Background()})
	require.ErrorIs(t, err, ErrRefusedBeforeChange)
	require.ErrorIs(t, err, ErrStagingExportRefused)
	var re *RefusalError
	require.ErrorAs(t, err, &re)
	require.Contains(t, err.Error(), "[pipeline_active_ref]")
	require.Contains(t, err.Error(), "agent-owner")
	require.False(t, schema.WardenDBPresent(d))
	matches, _ := filepath.Glob(filepath.Join(d, ".schema2-staging-*"))
	require.Empty(t, matches, "the refused staging scratch dir is removed")
	for rel, want := range before {
		require.Equal(t, want, snapshotTree(t, d)[rel], "%s changed", rel)
	}
}

func TestCutover_ApplyReconcilesDeletedPipelineHistoryEndToEnd(t *testing.T) {
	d := t.TempDir()
	seedRealisticLegacy(t, d)
	ctx := context.Background()
	s, err := storage.OpenLegacy(d)
	require.NoError(t, err)
	gone := reconcilePipe("plan-gone", pipeline.StatusDone)
	require.NoError(t, s.Pipelines.Create(&gone))
	writeLegacyPipelineFile(t, d, gone)
	require.NoError(t, s.Pipelines.Delete("plan-gone"))
	hist := &agentstore.Agent{ID: "agent-hist", Name: "hist", Type: store.TypeDevelopment, Status: store.StatusIdle, TmuxSession: "tmux-hist",
		ProjectID: rfProject, PipelineID: "plan-gone", JobID: "alpha", CreatedAt: rfNow, UpdatedAt: rfNow}
	require.NoError(t, s.Agents.Create(ctx, hist))
	require.NoError(t, s.Agents.Archive(ctx, "agent-hist"))
	require.NoError(t, s.Close())
	before := snapshotTree(t, d)

	require.NoError(t, NewRunner(DefaultRegistry).Apply(Env{DataDir: d, BinaryVersion: "11.2.0", Context: ctx}, schema.Schema2))
	after := snapshotTree(t, d)
	for rel, want := range before {
		if rel != schema.FileName {
			require.Equal(t, want, after[rel], "legacy file %s changed", rel)
		}
	}

	rt, err := storage.OpenSchema2ForMigrationVerification(d)
	require.NoError(t, err)
	defer rt.Close()
	require.NotNil(t, rt.DB)
	closed, err := rt.Agents.ListClosed(ctx)
	require.NoError(t, err)
	var found *agentstore.Agent
	for _, a := range closed {
		if a.ID == "agent-hist" {
			found = a
		}
	}
	require.NotNil(t, found, "archived history survives the cutover")
	require.Empty(t, found.PipelineID)
	require.NotNil(t, found.HistoricalPipeline, "the runtime reads the provenance back")
	require.Equal(t, "plan-gone", found.HistoricalPipeline.ID)
	require.Equal(t, "alpha", found.HistoricalPipeline.JobID)
	_, perr := rt.Pipelines.Get("plan-gone")
	require.Error(t, perr, "the deleted pipeline is not resurrected")
	live, err := rt.Pipelines.Get("pipe-1")
	require.NoError(t, err)
	require.Len(t, live.Jobs, 2)

	// Rollback: the same crash-after-promotion state restores to a schema-1 dir
	// whose pipeline history is exactly what it was, and a second cutover
	// reaches the same reconciled result.
	done, err := schema.Load(d)
	require.NoError(t, err)
	backup := done.History[len(done.History)-1].Backup
	require.NoError(t, rt.Close())
	rn := NewRunner(DefaultRegistry)
	env := Env{DataDir: d, BinaryVersion: "11.2.0", Context: ctx}
	require.NoError(t, schema.Save(d, &schema.Ledger{SchemaVersion: 1, BinaryVersion: "10.0.3",
		InProgress: &schema.InProgress{Migration: Migration0002ID, Step: "promoted", Snapshot: backup}}))
	require.NoError(t, rn.Restore(env))
	require.False(t, schema.WardenDBPresent(d))
	afterRestore := snapshotTree(t, d)
	for rel, want := range before {
		if rel != schema.FileName {
			require.Equal(t, want, afterRestore[rel], "restore must return legacy file %s byte-for-byte", rel)
		}
	}
	time.Sleep(1100 * time.Millisecond) // backup dirs are second-stamped and never reused
	require.NoError(t, rn.Apply(env, schema.Schema2))
	again, err := storage.OpenSchema2ForMigrationVerification(d)
	require.NoError(t, err)
	defer again.Close()
	closed, err = again.Agents.ListClosed(ctx)
	require.NoError(t, err)
	var n int
	for _, a := range closed {
		if a.ID == "agent-hist" && a.HistoricalPipeline != nil {
			n++
		}
	}
	require.Equal(t, 1, n)
}
