package pipeline

import (
	"testing"

	"github.com/srjn45/scriva"
	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/scriva/query"
	"github.com/srjn45/warden/internal/wardendb"
	"github.com/stretchr/testify/require"
)

func schema3PipelineFixture(t *testing.T) (*Store, *wardendb.DB, *engine.Collection) {
	t.Helper()
	db, err := wardendb.Open(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	for _, name := range []string{"projects", "agents"} {
		c, err := wardendb.Register[map[string]any](db, wardendb.Spec{Name: name, Version: 3, Options: []scriva.CollectionOption{scriva.WithUniqueIndex("id")}})
		require.NoError(t, err)
		if name == "projects" {
			eng, err := c.Engine()
			require.NoError(t, err)
			_, _, err = eng.InsertWithKey("p", map[string]any{"id": "p", "status": "open"})
			require.NoError(t, err)
		}
	}
	s, err := NewSchema3StoreFromDB(db)
	require.NoError(t, err)
	c, err := wardendb.Lookup[map[string]any](db, "agents")
	require.NoError(t, err)
	agents, err := c.Engine()
	require.NoError(t, err)
	return s, db, agents
}

func TestSchema3PipelineMaterializedFullJobsAndAttemptSelection(t *testing.T) {
	s, db, agents := schema3PipelineFixture(t)
	p := &Pipeline{ID: "pipe", ProjectID: "p", Name: "fixture", Status: StatusPending, Jobs: []Job{
		{ID: "z", Prompt: "full business prompt", Worktree: "fresh", Handoff: "retain handoff", Role: "worker", Backend: "codex", Model: "model", Status: JobPending, Output: "retained output", RestartBranch: "retained-restart", AutoRetryCount: 3},
		{ID: "a", Prompt: "second", DependsOn: []string{"z"}, Status: JobPending},
	}}
	require.NoError(t, s.Create(p))
	got, err := s.Get("pipe")
	require.NoError(t, err)
	require.Equal(t, p.Jobs, got.Jobs)
	_, _, err = agents.InsertWithKey("past", map[string]any{"id": "past", "project_id": "p", "pipeline_id": "pipe", "job_id": "z", "status": "done", "lifecycle": "archived", "created_at": "2026-01-01T00:00:00Z"})
	require.NoError(t, err)
	_, _, err = agents.InsertWithKey("current", map[string]any{"id": "current", "project_id": "p", "pipeline_id": "pipe", "job_id": "z", "status": "working", "lifecycle": "active", "created_at": "2026-01-02T00:00:00Z"})
	require.NoError(t, err)
	require.NoError(t, s.Update("pipe", func(next *Pipeline) { next.Jobs[0].Status = JobRunning; next.Jobs[0].SetAgentID("current") }))
	got, err = s.Get("pipe")
	require.NoError(t, err)
	require.Equal(t, "current", got.Jobs[0].AgentRef())
	col, err := wardendb.Lookup[JobRecord](db, "pipeline_jobs")
	require.NoError(t, err)
	eng, err := col.Engine()
	require.NoError(t, err)
	rows, err := eng.Scan(query.MatchAll)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	for _, row := range rows {
		require.NotContains(t, row.Data, "agent_id")
		require.NotContains(t, row.Data, "session_id")
		require.Equal(t, "p", row.Data["project_id"])
	}
	headers, err := wardendb.Lookup[pipelineHeader](db, "pipelines")
	require.NoError(t, err)
	headerEng, err := headers.Engine()
	require.NoError(t, err)
	raw, err := headerEng.GetByKey("pipe")
	require.NoError(t, err)
	for _, field := range []string{"jobs", "job_count", "jobs_done"} {
		require.NotContains(t, raw.Data, field)
	}
	before := *got
	require.Error(t, s.Update("pipe", func(next *Pipeline) { next.ParentAgentID = "current" }))
	require.Error(t, s.Update("pipe", func(next *Pipeline) { next.Jobs[0], next.Jobs[1] = next.Jobs[1], next.Jobs[0] }))
	got, err = s.Get("pipe")
	require.NoError(t, err)
	require.Equal(t, before, *got)
	require.NoError(t, s.Update("pipe", func(next *Pipeline) { next.Jobs[0].Status = JobPending; next.Jobs[0].SetAgentID("") }))
	got, err = s.Get("pipe")
	require.NoError(t, err)
	require.Empty(t, got.Jobs[0].AgentRef(), "a retry must not select a historical assignment")
}
