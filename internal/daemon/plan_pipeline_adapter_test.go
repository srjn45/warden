package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/ctxstore"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/planstore"
	"github.com/srjn45/warden/internal/projectstore"
)

func TestPipelineDisplayName(t *testing.T) {
	require.Equal(t, "P:my-plan", pipelineDisplayName("my-plan"))
	require.Equal(t, "P:my-plan", pipelineDisplayName("P:my-plan"))
	require.Equal(t, "P:unnamed", pipelineDisplayName(""))
	require.Equal(t, "P:unnamed", pipelineDisplayName("  "))
}

// planPipelineServer wires plans + projects + a shared agent store into an
// Executor with the PlanPipelineHook so job lifecycle updates Plan evidence.
func planPipelineServer(t *testing.T) (*httptest.Server, *Server, *planstore.Store, *pipeline.Store, string) {
	t.Helper()
	root := t.TempDir()
	gitInit(t, root)

	ps, err := planstore.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = ps.Close() })
	projects, err := projectstore.NewStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = projects.Close() })
	_, err = projects.OpenProject(root, "test", root)
	require.NoError(t, err)

	pips, err := pipeline.NewStore(t.TempDir())
	require.NoError(t, err)
	cs, err := ctxstore.New(t.TempDir())
	require.NoError(t, err)

	ss := newFakeStore()
	fl := &fakeLife{}
	exec := NewExecutor(pips, ss, fl, cs, func() {})
	srv := &Server{store: ss, life: fl, plans: ps, exec: exec, projects: projects}
	exec.SetPlanPipelineHook(srv)
	ts := httptest.NewServer(srv.router())
	t.Cleanup(ts.Close)
	return ts, srv, ps, pips, root
}

// seedTwoJobPlanYAML writes a plan with t1 → t2 dependency chain.
func seedTwoJobPlanYAML(t *testing.T, root, subpath, name string) {
	t.Helper()
	abs := filepath.Join(root, subpath)
	require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o755))
	content := fmt.Sprintf(`version: 1
name: %s
goal: two-job chain
tasks:
  - id: t1
    prompt: first task
  - id: t2
    prompt: second task
    after: [t1]
`, name)
	require.NoError(t, os.WriteFile(abs, []byte(content), 0o644))
	gitAdd(t, root, subpath)
	gitCommit(t, root, "add plan "+name)
}

// TestPlanPipelineAdapter_TwoJobDependencyChain is the E2E for mode=pipeline:
// creates P:<name>, PlanID, identity task→job map, then drives emit t1→t2 and
// verifies PlanExecutionEvents + task evidence via the Plan service.
func TestPlanPipelineAdapter_TwoJobDependencyChain(t *testing.T) {
	ts, _, plans, pips, root := planPipelineServer(t)
	ctx := context.Background()

	seedTwoJobPlanYAML(t, root, "plans/pending/chain-plan.yaml", "chain-plan")
	id := planstore.PlanID(root, "chain-plan")
	require.NoError(t, plans.Create(ctx, &planstore.Plan{
		ID: id, ProjectID: root, Name: "chain-plan", Goal: "two-job chain",
		Tasks: []planstore.PlanTask{
			{ID: "t1", Prompt: "first task"},
			{ID: "t2", Prompt: "second task", After: []string{"t1"}},
		},
		FilePath: "plans/pending/chain-plan.yaml", Status: planstore.PlanStatusPending,
		TaskProgress: map[string]string{"t1": "pending", "t2": "pending"},
	}))

	resp := postJSON(t, planURL(ts.URL, root, "/"+id+"/run"), map[string]any{"mode": "pipeline"})
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))

	var got planstore.Plan
	require.NoError(t, json.NewDecoder(bytes.NewReader(body)).Decode(&got))
	require.Equal(t, planstore.PlanStatusInProgress, got.Status)
	require.Equal(t, planstore.PlanModePipeline, got.ExecutionMode)
	require.Equal(t, id, got.PipelineID)

	pl, err := pips.Get(got.PipelineID)
	require.NoError(t, err)
	require.Equal(t, "P:chain-plan", pl.Name)
	require.Equal(t, id, pl.PlanID)
	require.Equal(t, id, pl.ID)
	require.Len(t, pl.Jobs, 2)
	require.Equal(t, "t1", pl.Jobs[0].ID)
	require.Equal(t, "t2", pl.Jobs[1].ID)
	require.Equal(t, []string{"t1"}, pl.Jobs[1].DependsOn)

	// ActiveExecution evidence: task→job identity map.
	p, err := plans.Get(ctx, id)
	require.NoError(t, err)
	require.NotNil(t, p.ActiveExecution)
	require.Equal(t, planstore.PlanModePipeline, p.ActiveExecution.ExecutionMode)
	require.Equal(t, pl.ID, p.ActiveExecution.ExecutorID)
	require.Equal(t, map[string]string{"t1": "t1", "t2": "t2"}, p.ActiveExecution.TaskJobMap)

	events, err := plans.ListEvents(ctx, id, p.ActiveExecution.ID)
	require.NoError(t, err)
	kinds := map[planstore.EventKind]int{}
	for _, ev := range events {
		kinds[ev.Kind]++
	}
	require.Equal(t, 1, kinds[planstore.EventKindExecutionStarted])
	require.Equal(t, 1, kinds[planstore.EventKindExecutorCreated])
	require.Equal(t, 1, kinds[planstore.EventKindTaskAssigned], "t1 assigned on first reconcile")
	require.Equal(t, 1, kinds[planstore.EventKindAgentSpawned])

	require.Equal(t, pipeline.JobRunning, pl.Job("t1").Status)
	require.Equal(t, pipeline.JobPending, pl.Job("t2").Status)

	p, err = plans.Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "in_progress", p.TaskProgress["t1"])

	// Complete t1 → unlocks t2.
	emitBody, _ := json.Marshal(map[string]any{"text": "t1 done"})
	emitResp, err := http.Post(ts.URL+"/api/v1/pipelines/"+id+"/jobs/t1/emit", "application/json", bytes.NewReader(emitBody))
	require.NoError(t, err)
	defer emitResp.Body.Close()
	emitOut, _ := io.ReadAll(emitResp.Body)
	require.Equal(t, http.StatusOK, emitResp.StatusCode, string(emitOut))

	pl, err = pips.Get(id)
	require.NoError(t, err)
	require.Equal(t, pipeline.JobDone, pl.Job("t1").Status)
	require.Equal(t, pipeline.JobRunning, pl.Job("t2").Status)

	p, err = plans.Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "done", p.TaskProgress["t1"])
	require.Equal(t, "in_progress", p.TaskProgress["t2"])
	require.Equal(t, "done", p.TaskOutcomes["t1"].Status)

	events, err = plans.ListEvents(ctx, id, p.ActiveExecution.ID)
	require.NoError(t, err)
	kinds = map[planstore.EventKind]int{}
	for _, ev := range events {
		kinds[ev.Kind]++
	}
	require.GreaterOrEqual(t, kinds[planstore.EventKindTaskEvidenceVerified], 1)
	require.Equal(t, 2, kinds[planstore.EventKindTaskAssigned])
	require.Equal(t, 2, kinds[planstore.EventKindAgentSpawned])

	emitBody2, _ := json.Marshal(map[string]any{"text": "t2 done"})
	emitResp2, err := http.Post(ts.URL+"/api/v1/pipelines/"+id+"/jobs/t2/emit", "application/json", bytes.NewReader(emitBody2))
	require.NoError(t, err)
	defer emitResp2.Body.Close()
	emitOut2, _ := io.ReadAll(emitResp2.Body)
	require.Equal(t, http.StatusOK, emitResp2.StatusCode, string(emitOut2))

	pl, err = pips.Get(id)
	require.NoError(t, err)
	require.Equal(t, pipeline.StatusDone, pl.Status)
	require.Equal(t, pipeline.JobDone, pl.Job("t2").Status)

	p, err = plans.Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "done", p.TaskProgress["t2"])
	require.Equal(t, planstore.ExecutionStatusCompleted, p.ActiveExecution.TerminalStatus)

	events, err = plans.ListEvents(ctx, id, p.ActiveExecution.ID)
	require.NoError(t, err)
	kinds = map[planstore.EventKind]int{}
	for _, ev := range events {
		kinds[ev.Kind]++
	}
	require.Equal(t, 1, kinds[planstore.EventKindCompletionVerified])
	require.Equal(t, 2, kinds[planstore.EventKindTaskEvidenceVerified])
}

// TestPlanPipelineAdapter_PlanlessPipelineUnchanged verifies independent
// CreatePipeline still works without PlanID and does not write plan events.
func TestPlanPipelineAdapter_PlanlessPipelineUnchanged(t *testing.T) {
	ts, _, plans, _, root := planPipelineServer(t)
	projDir := root

	spec := "name: arbitrary\nrepo: " + projDir + "\njobs:\n  - id: a\n    prompt: go\n    worktree: none\n"
	body, _ := json.Marshal(map[string]any{"project_id": root, "spec": spec})
	resp, err := http.Post(ts.URL+"/api/v1/pipelines", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusCreated, resp.StatusCode, string(raw))

	var pl pipeline.Pipeline
	require.NoError(t, json.Unmarshal(raw, &pl))
	require.Equal(t, "arbitrary", pl.Name)
	require.Empty(t, pl.PlanID)

	// No plan events should exist for any plan (store empty of executions).
	list, err := plans.ListByProject(context.Background(), root)
	require.NoError(t, err)
	for _, p := range list {
		require.Nil(t, p.ActiveExecution)
	}
}

// TestPlansRunPipelineMode_NamedAndMapped updates the classic run-mode cover
// for P:<name> + TaskJobMap evidence.
func TestPlansRunPipelineMode_NamedAndMapped(t *testing.T) {
	ts, _, plans, pips, root := planPipelineServer(t)
	ctx := context.Background()

	seedPlanYAML(t, root, "plans/pending/pipeline-plan.yaml", "pipeline-plan")
	id := planstore.PlanID(root, "pipeline-plan")
	require.NoError(t, plans.Create(ctx, &planstore.Plan{
		ID: id, ProjectID: root, Name: "pipeline-plan", Goal: "test",
		Tasks:    []planstore.PlanTask{{ID: "t1", Prompt: "task 1"}},
		FilePath: "plans/pending/pipeline-plan.yaml", Status: planstore.PlanStatusPending,
		TaskProgress: map[string]string{"t1": "pending"},
	}))

	resp := postJSON(t, planURL(ts.URL, root, "/"+id+"/run"), map[string]any{"mode": "pipeline"})
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))

	var got planstore.Plan
	require.NoError(t, json.NewDecoder(bytes.NewReader(body)).Decode(&got))
	require.Equal(t, id, got.PipelineID)

	pl, err := pips.Get(got.PipelineID)
	require.NoError(t, err)
	require.Equal(t, "P:pipeline-plan", pl.Name)
	require.Equal(t, id, pl.PlanID)
	require.NotEmpty(t, pl.Jobs)

	p, err := plans.Get(ctx, id)
	require.NoError(t, err)
	require.NotNil(t, p.ActiveExecution)
	require.NotEmpty(t, p.ActiveExecution.TaskJobMap)
	for _, j := range pl.Jobs {
		require.Equal(t, j.ID, p.ActiveExecution.TaskJobMap[j.ID])
	}
}

// TestBuildPlanPipeline_UsesLiveDefinitionNotStaleSnapshot ensures a re-run
// after editing task after-deps while pending picks up the live DAG, even when
// a sealed ActiveExecution from a prior attempt still holds an older snapshot.
func TestBuildPlanPipeline_UsesLiveDefinitionNotStaleSnapshot(t *testing.T) {
	p := &planstore.Plan{
		ID:   "plan-deadbeef",
		Name: "dag-plan",
		Goal: "ordered",
		Tasks: []planstore.PlanTask{
			{ID: "t1", Prompt: "first"},
			{ID: "t2", Prompt: "second", After: []string{"t1"}},
		},
		ActiveExecution: &planstore.PlanExecution{
			ID:             "pe-old",
			TerminalStatus: planstore.ExecutionStatusFailed,
			Snapshot: &planstore.ExecutionSnapshot{
				Name: "dag-plan",
				Goal: "ordered",
				// Stale: no after edges (as if created before deps were patched).
				Tasks: []planstore.PlanTask{
					{ID: "t1", Prompt: "first"},
					{ID: "t2", Prompt: "second"},
				},
			},
		},
	}
	pl, taskJob, err := buildPlanPipeline(p, "/tmp/repo")
	require.NoError(t, err)
	require.Equal(t, map[string]string{"t1": "t1", "t2": "t2"}, taskJob)
	require.Len(t, pl.Jobs, 2)
	require.Empty(t, pl.Jobs[0].DependsOn)
	require.Equal(t, []string{"t1"}, pl.Jobs[1].DependsOn)
}
