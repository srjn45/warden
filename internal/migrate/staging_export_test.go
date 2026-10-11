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
	require.Equal(t, 2, rep.Records["agents"])
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
	require.NotContains(t, project, "agents")
	require.NotContains(t, project, "pipelines")
	require.NotContains(t, project, "terminals")
	require.Equal(t, "group-1", project["group_id"])
	require.Equal(t, float64(0), project["group_order"])
	require.Equal(t, "2026-10-11T12:00:00Z", project["created_at"])

	agent := stagingRecord(t, db, "agents", "agent-1")
	require.NotContains(t, agent, "child_agents")
	require.NotContains(t, agent, "child_pipelines")
	require.NotContains(t, agent, "child_autopilots")
	require.Equal(t, "agent-1", agent["id"])
	require.Equal(t, "2026-10-11T12:00:00Z", agent["created_at"])

	archived := stagingRecord(t, db, "agents", "agent-old")
	require.Equal(t, "archived", archived["status"])
	require.Equal(t, "done", archived["archived_from_status"])
	require.Equal(t, "2026-10-11T12:00:00Z", archived["archived_at"])

	pipe := stagingRecord(t, db, "pipelines", "pipe-1")
	require.NotContains(t, pipe, "jobs")
	job := stagingRecord(t, db, "pipeline_jobs", "pipe-1:build")
	require.Equal(t, "pipe-1", job["pipeline_id"])
	require.Equal(t, "build", job["job_id"])
	require.Equal(t, float64(0), job["seq"])
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
	require.Equal(t, 2, rep.SourceCounts["agents"])

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
	writeScrivaRecord(t, filepath.Join(d, "agents-db"), "archived", "agent-old", agentstore.Agent{
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
