package migrate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srjn45/scriva"
	"github.com/srjn45/scriva/engine"
	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/projectstore"
	"github.com/srjn45/warden/internal/store"
	"github.com/srjn45/warden/internal/terminalstore"
	"github.com/srjn45/warden/internal/wardendb"
)

func TestExportSchema2Staging_IdempotentImmutableAndNormalizes(t *testing.T) {
	d := stagingLegacyDir(t)
	before := snapshotTree(t, d)
	target := filepath.Join(t.TempDir(), "staging")

	rep, err := ExportSchema2Staging(context.Background(), d, StagingExportOptions{TargetDir: target, Now: t0})
	require.NoError(t, err)
	require.False(t, rep.Blocked, "%+v", rep.Findings)
	require.Equal(t, 1, rep.Records["projects"])
	require.Equal(t, 1, rep.Records["project_groups"])
	require.Equal(t, 1, rep.Records["agents"])
	require.Equal(t, 1, rep.Records["archived_agents"], "schema-1 agents-db/closed maps 1:1 to archived_agents")
	require.Equal(t, 1, rep.Records["terminals"])
	require.Equal(t, 1, rep.Records["pipelines"])
	require.Equal(t, 2, rep.Records["pipeline_jobs"])
	require.Equal(t, before, snapshotTree(t, d), "export must not mutate legacy sources")

	again, err := ExportSchema2Staging(context.Background(), d, StagingExportOptions{TargetDir: target, Now: t0})
	require.NoError(t, err)
	require.Equal(t, rep.Records, again.Records)
	require.Equal(t, rep.Checksums, again.Checksums)
	require.Equal(t, before, snapshotTree(t, d), "idempotent rerun must also leave legacy sources alone")

	db, err := scriva.Open(target, scriva.WithSyncMode(engine.SyncModeNone))
	require.NoError(t, err)
	defer db.Close()

	project := stagingRecord(t, db, "projects", "/repo")
	// The shared runtime stores are still schema-1 shaped and read these
	// lists as authoritative, so the export carries them over verbatim.
	require.Equal(t, []any{"agent-1"}, project["agents"])
	require.Equal(t, []any{"pipe-1"}, project["pipelines"])
	require.Equal(t, []any{"term-1"}, project["terminals"])
	require.Equal(t, "group-1", project["group_id"])
	require.Equal(t, float64(0), project["group_order"])
	require.Equal(t, "2026-10-11T12:00:00Z", project["created_at"])

	agent := stagingRecord(t, db, "agents", "agent-1")
	require.Equal(t, []any{"child"}, agent["child_agents"])
	require.Equal(t, []any{"pipe-child"}, agent["child_pipelines"])
	require.Equal(t, []any{"ap-child"}, agent["child_autopilots"])
	require.Equal(t, "agent-1", agent["id"])
	require.Equal(t, "2026-10-11T12:00:00Z", agent["created_at"])

	archived := stagingRecord(t, db, "archived_agents", "agent-old")
	require.Equal(t, "done", archived["status"], "archived record is carried over unchanged")

	pipe := stagingRecord(t, db, "pipelines", "pipe-1")
	require.Len(t, pipe["jobs"], 2, "jobs stay embedded for the schema-1 shaped pipeline store")
	job := stagingRecord(t, db, "pipeline_jobs", "pipe-1:build")
	require.Equal(t, "pipe-1", job["pipeline_id"])
	require.Equal(t, "build", job["job_id"])
	require.Equal(t, float64(0), job["seq"])
}

func TestExportSchema2Staging_ExportsMappedMigratedSources(t *testing.T) {
	d := stagingLegacyDir(t)
	writeScrivaRecord(t, filepath.Join(d, "plans", "plans-db"), "plans", "plan-1", map[string]any{"id": "plan-1", "project_id": "/repo", "status": "pending"})
	writeScrivaRecord(t, filepath.Join(d, "plans", "plans-db"), "plan-events", "evt-1", map[string]any{"id": "evt-1", "plan_id": "plan-1"})
	writeScrivaRecord(t, filepath.Join(d, "plans", "plans-db"), "plan-notes", "note-1", map[string]any{"id": "note-1", "plan_id": "plan-1"})
	writeScrivaRecord(t, filepath.Join(d, "plans", "plan-exports-db"), "plan-exports", "export-1", map[string]any{"id": "export-1", "plan_id": "plan-1"})
	writeScrivaRecord(t, filepath.Join(d, "autopilot", "runs-db"), "autopilot_runs", "ap-123456789abc", map[string]any{"id": "ap-123456789abc", "project_id": "/repo", "plan_id": "plan-1", "state": "running"})
	writeScrivaRecord(t, filepath.Join(d, "context"), "context", "ctx-1", map[string]any{"id": "ctx-1", "namespace": "n", "key": "k", "value": "v"})
	writeScrivaRecord(t, filepath.Join(d, "inbox"), "messages", "msg-1", map[string]any{"id": "msg-1", "to": "agent-1", "body": "hi"})
	writeScrivaRecord(t, filepath.Join(d, "inbox"), "id_sequences", "messages", map[string]any{"id": "messages", "next": 2})
	writeScrivaRecord(t, filepath.Join(d, "backends"), "models", "model-1", map[string]any{"id": "model-1", "backend": "codex"})
	writeScrivaRecord(t, filepath.Join(d, "snapshots-db"), "snapshots", "snap-1", map[string]any{"id": "snap-1", "agent_id": "agent-1"})
	writeScrivaRecord(t, filepath.Join(d, "schedules-db"), "schedules", "sched-1", map[string]any{"id": "sched-1", "mode": "once"})
	// Schema-1 flat sources, in the exact formats the compat stores write.
	writeFile(t, d, "usage-snapshots/usage-snapshots.jsonl", `{"revision":7,"domain":{"provider":"codex"},"observed_at":"2026-10-11T12:00:00Z"}`+"\n")
	writeFile(t, d, "quota-impact/quota-impact-fences.jsonl", `{"snapshot_revision":7,"domain_key":"codex","bucket_key":"5h","agent_id":"agent-1","recovery_generation":1}`+"\n")
	writeFile(t, d, "savings/ledger.jsonl", `{"ts":"2026-10-11T12:00:00Z","feature":"compact","raw_tokens":9,"kept_tokens":3,"saved":6}`+"\n")
	writeFile(t, d, "savings/calibration.json", `{"bytes_per_token":3.5,"samples":1}`)
	writeFile(t, d, "spend/spend.json", `{"agent-1":{"input":10,"output":4,"backend":"claude"}}`)
	writeFile(t, d, "metrics/2026-10-11.jsonl", `{"taken_at":"2026-10-11T12:00:00Z","system":{},"agents":[],"daemon":{}}`+"\n")
	writeFile(t, d, "audit.jsonl", `{"time":"2026-10-11T12:00:00Z","action":"spawn","target":"agent-1"}`+"\n"+`{"time":"2026-10-11T11:00:00Z","action":"late-clock","target":"agent-1"}`+"\n")
	require.NoError(t, os.WriteFile(filepath.Join(d, "schedules.json"), []byte(`{"sched-flat":{"id":"sched-flat","mode":"agent"}}`), 0o600))
	writeScrivaRecord(t, filepath.Join(d, "known-prompts-db"), "known_prompts", "prompt-1", map[string]any{"id": "prompt-1", "prompt": "p"})
	require.NoError(t, os.MkdirAll(filepath.Join(d, "plan-sync-hub"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(d, "plan-sync-hub", "env.json"), []byte(`{"id":"env-1","plan_id":"plan-1"}`), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(d, "snapshots"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(d, "snapshots", "snap-flat.json"), []byte(`{"id":"snap-flat","agent_id":"agent-1","transcript_path":"snapshots/snap-flat.transcript"}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(d, "snapshots", "snap-flat.transcript"), []byte("kept external"), 0o600))

	target := filepath.Join(t.TempDir(), "staging")
	rep, err := ExportSchema2Staging(context.Background(), d, StagingExportOptions{TargetDir: target, Now: t0})
	require.NoError(t, err, "%+v", rep.Findings)
	require.False(t, rep.Blocked, "%+v", rep.Findings)
	for col, count := range map[string]int{
		"plans": 1, "plan_events": 1, "plan_notes": 1, "plan_exports": 1, "autopilot_runs": 1, "context_entries": 1,
		"messages": 1, "id_sequences": 1, "backend_models": 1, "usage_snapshots": 1, "quota_impact_fences": 1, "audit_events": 2,
		"snapshots": 2, "savings_events": 1, "savings_calibration": 1, "spend_records": 1, "schedules": 2,
		"known_prompts": 1, "metrics_samples": 1, "plan_sync_envelopes": 1,
	} {
		require.Equal(t, count, rep.Records[col], col)
	}

	db, err := scriva.Open(target, scriva.WithSyncMode(engine.SyncModeNone))
	require.NoError(t, err)
	require.Equal(t, "ctx-1", stagingRecord(t, db, "context_entries", "ctx-1")["id"])
	require.Equal(t, "env-1", stagingRecord(t, db, "plan_sync_envelopes", "env-1")["id"])
	require.Equal(t, "snap-flat", stagingRecord(t, db, "snapshots", "snap-flat")["id"])
	require.NoError(t, db.Close())

	eq, err := VerifySchema2Equivalence(context.Background(), d, target)
	require.NoError(t, err)
	require.True(t, eq.Passed, "%+v", eq.Findings)
	require.Equal(t, rep.Records, eq.SourceCounts)
}

func TestExportSchema2Staging_BlocksMigratedSourceWithoutSafeMapping(t *testing.T) {
	d := stagingLegacyDir(t)
	writeScrivaRecord(t, filepath.Join(d, "sessions-db"), "agents", "agent-from-session", map[string]any{"id": "agent-from-session"})

	rep, err := ExportSchema2Staging(context.Background(), d, StagingExportOptions{TargetDir: filepath.Join(t.TempDir(), "staging"), Now: t0})
	require.ErrorIs(t, err, ErrStagingExportRefused)
	require.True(t, rep.Blocked)
	requireBlocking(t, rep.Findings, "sessions-db", "no safe staging mapping")
}

// sessions-db and the flat sessions/ + closed/ JSON are import-time copies that
// the boot/0001 importers already moved into agents-db / terminals-db. They must
// not block the cutover (every real upgraded install has them) and must not be
// re-imported (that would resurrect deleted agents) — but records that exist
// only there are reported, never dropped silently.
func TestExportSchema2Staging_LegacySessionCopiesAreReconciledNotBlocking(t *testing.T) {
	d := stagingLegacyDir(t)
	sdb := filepath.Join(d, "sessions-db")
	writeScrivaRecord(t, sdb, "active", "agent-1", map[string]any{"id": "agent-1", "status": "working"})  // covered
	writeScrivaRecord(t, sdb, "closed", "agent-old", map[string]any{"id": "agent-old", "status": "done"}) // covered (archived)
	writeScrivaRecord(t, sdb, "active", "term-1", map[string]any{"id": "term-1", "status": "working"})    // covered (terminal)
	writeScrivaRecord(t, sdb, "closed", "gone-1", map[string]any{"id": "gone-1", "status": "done"})       // deleted since the import
	writeFile(t, d, "sessions/gone-2.json", `{"id":"gone-2"}`)
	writeFile(t, d, "closed/agent-old.json", `{"id":"agent-old"}`)
	before := snapshotTree(t, d)

	rep, err := ExportSchema2Staging(context.Background(), d, StagingExportOptions{TargetDir: filepath.Join(t.TempDir(), "staging"), Now: t0})
	require.NoError(t, err, "%+v", rep.Findings)
	require.False(t, rep.Blocked)
	// Nothing re-imported: destination counts are exactly agents-db/terminals-db.
	require.Equal(t, 1, rep.Records["agents"])
	require.Equal(t, 1, rep.Records["archived_agents"])
	var note *Finding
	for i, f := range rep.Findings {
		if f.Store == "sessions-db" && f.Severity == SeverityRepairable {
			note = &rep.Findings[i]
		}
	}
	require.NotNil(t, note, "records only present in the legacy copies must be reported")
	require.Contains(t, note.Message, "2 legacy session record(s)")
	require.Contains(t, note.Message, "gone-1")
	require.Contains(t, note.Message, "gone-2")
	require.NotContains(t, note.Message, "agent-old")
	require.Equal(t, before, snapshotTree(t, d), "sources are only read")
}

func TestExportSchema2Staging_BlocksUnmappedCollectionsInMappedSource(t *testing.T) {
	d := stagingLegacyDir(t)
	writeScrivaRecord(t, filepath.Join(d, "plans", "plans-db"), "plans", "plan-1", map[string]any{"id": "plan-1", "project_id": "/repo"})
	writeScrivaRecord(t, filepath.Join(d, "plans", "plans-db"), "surprise", "surprise-1", map[string]any{"id": "surprise-1"})

	rep, err := ExportSchema2Staging(context.Background(), d, StagingExportOptions{TargetDir: filepath.Join(t.TempDir(), "staging"), Now: t0})
	require.ErrorIs(t, err, ErrStagingExportRefused)
	require.True(t, rep.Blocked)
	requireBlocking(t, rep.Findings, "plans/plans-db", "surprise")
}

func TestExportSchema2Staging_RefusesRelationshipViolations(t *testing.T) {
	d := stagingLegacyDir(t)
	writeScrivaRecord(t, filepath.Join(d, "pipelines-db"), "pipelines", "bad", pipeline.Pipeline{
		ID: "bad", Name: "bad", ProjectID: "missing-project",
		Jobs: []pipeline.Job{{ID: "only", DependsOn: []string{"ghost"}}},
	})

	rep, err := ExportSchema2Staging(context.Background(), d, StagingExportOptions{TargetDir: filepath.Join(t.TempDir(), "staging"), Now: t0})
	require.ErrorIs(t, err, ErrStagingExportRefused)
	require.True(t, rep.Blocked)
	requireBlocking(t, rep.Findings, "pipelines", "missing-project")
	requireBlocking(t, rep.Findings, "pipeline_jobs", "depends_on ghost")
}

func TestExportSchema2Staging_RefusesDivergentTarget(t *testing.T) {
	d := stagingLegacyDir(t)
	target := filepath.Join(t.TempDir(), "staging")
	_, err := ExportSchema2Staging(context.Background(), d, StagingExportOptions{TargetDir: target, Now: t0})
	require.NoError(t, err)

	db, err := scriva.Open(target, scriva.WithSyncMode(engine.SyncModeAlways))
	require.NoError(t, err)
	col, err := db.Collection("agents")
	require.NoError(t, err)
	_, err = col.Upsert("agent-1", map[string]any{"id": "agent-1", "subject": "tampered"})
	require.NoError(t, err)
	require.NoError(t, db.Close())

	rep, err := ExportSchema2Staging(context.Background(), d, StagingExportOptions{TargetDir: target, Now: t0})
	require.ErrorIs(t, err, ErrStagingExportRefused)
	require.True(t, rep.Blocked)
	requireBlocking(t, rep.Findings, "agents", "differs")
}

func TestVerifySchema2Equivalence_ReportsStableCountsHashesAndTampering(t *testing.T) {
	d := stagingLegacyDir(t)
	target := filepath.Join(t.TempDir(), "staging")
	_, err := ExportSchema2Staging(context.Background(), d, StagingExportOptions{TargetDir: target, Now: t0})
	require.NoError(t, err)

	rep, err := VerifySchema2Equivalence(context.Background(), d, target)
	require.NoError(t, err)
	require.True(t, rep.Passed, "%+v", rep.Findings)
	require.Equal(t, rep.SourceCounts, rep.TargetCounts)
	require.Equal(t, rep.SourceHashes, rep.TargetHashes)
	require.Equal(t, 1, rep.SourceCounts["agents"])
	require.Equal(t, 1, rep.SourceCounts["archived_agents"])

	db, err := scriva.Open(target, scriva.WithSyncMode(engine.SyncModeAlways))
	require.NoError(t, err)
	col, err := db.Collection("agents")
	require.NoError(t, err)
	_, err = col.Upsert("agent-1", map[string]any{"id": "agent-1", "subject": "tampered"})
	require.NoError(t, err)
	require.NoError(t, db.Close())

	rep, err = VerifySchema2Equivalence(context.Background(), d, target)
	require.ErrorIs(t, err, ErrStagingExportRefused)
	require.False(t, rep.Passed)
	requireBlocking(t, rep.Findings, "agents", "count or canonical hash differs")
}

func stagingLegacyDir(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	now := t0
	writeScrivaRecord(t, filepath.Join(d, "projects"), "projects", "/repo", projectstore.Project{
		ID: "/repo", Name: "repo", Path: "/repo", Status: projectstore.StatusOpen,
		Agents: []string{"agent-1"}, Pipelines: []string{"pipe-1"}, Terminals: []string{"term-1"},
		CreatedAt: now, UpdatedAt: now,
	})
	writeScrivaRecord(t, filepath.Join(d, "projects"), "project_groups", "group-1", projectstore.ProjectGroup{
		ID: "group-1", Name: "group", ProjectIDs: []string{"/repo"}, CreatedAt: now, UpdatedAt: now,
	})
	writeScrivaRecord(t, filepath.Join(d, "agents-db"), "agents", "agent-1", agentstore.Agent{
		ID: "agent-1", Name: "agent", Type: store.TypeDevelopment, Status: store.StatusWorking,
		TmuxSession: "tmux-agent", ProjectID: "/repo", PipelineID: "pipe-1", JobID: "build",
		ChildAgents: []string{"child"}, ChildPipelines: []string{"pipe-child"}, ChildAutopilots: []string{"ap-child"},
		CreatedAt: now, UpdatedAt: now,
	})
	writeScrivaRecord(t, filepath.Join(d, "agents-db"), "closed", "agent-old", agentstore.Agent{
		ID: "agent-old", Name: "old", Status: store.StatusDone, TmuxSession: "tmux-old",
		ProjectID: "/repo", CreatedAt: now, UpdatedAt: now,
	})
	writeScrivaRecord(t, filepath.Join(d, "terminals-db"), "terminals", "term-1", terminalstore.Terminal{
		ID: "term-1", ProjectID: "/repo", Name: "shell", TmuxSession: "tmux-term",
		Status: terminalstore.StatusRunning, CreatedAt: now, UpdatedAt: now,
	})
	writeScrivaRecord(t, filepath.Join(d, "pipelines-db"), "pipelines", "pipe-1", pipeline.Pipeline{
		ID: "pipe-1", Name: "pipe-1", Status: pipeline.StatusRunning, ProjectID: "/repo",
		Jobs: []pipeline.Job{
			{ID: "build", Status: pipeline.JobRunning, AgentID: "agent-1"},
			{ID: "test", Status: pipeline.JobPending, DependsOn: []string{"build"}},
		},
	})
	return d
}

func writeScrivaRecord(t *testing.T, dbDir, colName, key string, v any) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dbDir, 0o700))
	db, err := scriva.Open(dbDir, scriva.WithSyncMode(engine.SyncModeAlways))
	require.NoError(t, err)
	defer db.Close()
	col, err := db.Collection(colName)
	require.NoError(t, err)
	body, err := toMap(v)
	require.NoError(t, err)
	_, _, err = col.InsertWithKey(key, body)
	require.NoError(t, err)
}

func stagingRecord(t *testing.T, db *scriva.DB, colName, key string) map[string]any {
	t.Helper()
	col, err := db.Collection(colName)
	require.NoError(t, err)
	rec, err := col.GetByKey(key)
	require.NoError(t, err)
	delete(rec.Data, "_key")
	return rec.Data
}

func requireBlocking(t *testing.T, findings []Finding, storeName, contains string) {
	t.Helper()
	for _, f := range findings {
		if f.Severity == SeverityBlocking && f.Store == storeName && strings.Contains(f.Message, contains) {
			return
		}
	}
	t.Fatalf("missing blocking finding for %s containing %q: %+v", storeName, contains, findings)
}

func TestExportSchema2Staging_RefusesCutoverTarget(t *testing.T) {
	d := stagingLegacyDir(t)
	rep, err := ExportSchema2Staging(context.Background(), d, StagingExportOptions{TargetDir: filepath.Join(d, wardendb.DirName), Now: t0})
	require.ErrorIs(t, err, ErrStagingExportRefused)
	require.True(t, rep.Blocked)
	_, statErr := os.Stat(filepath.Join(d, wardendb.DirName))
	require.True(t, errors.Is(statErr, os.ErrNotExist))
}
