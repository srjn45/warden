package daemon

import (
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/daemon/oapi"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/planstore"
	"github.com/srjn45/warden/internal/store"
)

// newPipelineRestartPlan runs a four-task plan (a; b,d after a; c after b) in
// pipeline mode and returns the plan id (== pipeline id).
func newPipelineRestartPlan(t *testing.T) (srvURL string, srv *Server, plans *planstore.Store, pips *pipeline.Store, root, id string) {
	t.Helper()
	ts, srv, plans, pips, root := planPipelineServer(t)
	srv.cstore = srv.exec.cstore
	require.NoError(t, exec.Command("git", "-C", root, "commit", "--allow-empty", "-m", "init").Run())
	ctx := context.Background()
	rel := "plans/pending/rp.yaml"
	require.NoError(t, os.MkdirAll(filepath.Join(root, "plans/pending"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, rel), []byte(`version: 1
name: rp
goal: restart
tasks:
  - id: a
    prompt: do a
  - id: b
    prompt: do b
    after: [a]
  - id: c
    prompt: do c
    after: [b]
  - id: d
    prompt: do d
    after: [a]
`), 0o644))
	gitAdd(t, root, rel)
	gitCommit(t, root, "add plan")
	id = planstore.PlanID(root, "rp")
	require.NoError(t, plans.Create(ctx, &planstore.Plan{
		ID: id, ProjectID: root, Name: "rp", Goal: "restart", FilePath: rel, Status: planstore.PlanStatusPending,
		Tasks: []planstore.PlanTask{
			{ID: "a", Prompt: "do a"}, {ID: "b", Prompt: "do b", After: []string{"a"}},
			{ID: "c", Prompt: "do c", After: []string{"b"}}, {ID: "d", Prompt: "do d", After: []string{"a"}},
		},
		TaskProgress: map[string]string{"a": "pending", "b": "pending", "c": "pending", "d": "pending"},
	}))
	resp := postJSON(t, planURL(ts.URL, root, "/"+id+"/run"), map[string]any{"mode": "pipeline"})
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	return ts.URL, srv, plans, pips, root, id
}

func jobSpawns(f *fakeLife, job string) (n int, last string, base string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.jobReqs {
		if r.JobID == job {
			n++
			last, base = r.Prompt, r.BaseBranch
		}
	}
	return
}

// TestRestartPlanPipelineMixedJobs: done a is kept (not re-run, its handoff
// reaches the reset dependents), failed b with commits keeps its branch as the
// new worktree base, skipped c and unfinished d reset, and no old agent survives.
func TestRestartPlanPipelineMixedJobs(t *testing.T) {
	url, srv, plans, pips, root, id := newPipelineRestartPlan(t)
	ctx := context.Background()
	fl := srv.life.(*fakeLife)

	// a done with a handoff; b failed on a branch with a commit; c skipped.
	require.NoError(t, srv.exec.Emit(ctx, id, "a", "A-HANDOFF"))
	branchB := id + "-b"
	require.NoError(t, exec.Command("git", "-C", root, "branch", branchB).Run())
	require.NoError(t, exec.Command("git", "-C", root, "checkout", "-q", branchB).Run())
	require.NoError(t, exec.Command("git", "-C", root, "commit", "--allow-empty", "-m", "b work").Run())
	require.NoError(t, exec.Command("git", "-C", root, "checkout", "-q", "-").Run())
	require.NoError(t, srv.store.Delete(ctx, branchB)) // the run already spawned b's agent
	require.NoError(t, srv.store.Insert(ctx, &agentstore.Agent{
		ID: branchB, TmuxSession: branchB, Repo: root, PipelineID: id, JobID: "b",
		Status: store.StatusErrored, Worktree: ".worktrees/" + branchB, Branch: branchB, BranchCreated: true,
	}))
	require.NoError(t, pips.Update(id, func(p *pipeline.Pipeline) {
		p.Job("b").Status = pipeline.JobFailed
		p.Job("b").SetAgentID(branchB)
		p.Job("c").Status = pipeline.JobSkipped
		p.Job("d").Status = pipeline.JobSkipped
		p.Status = pipeline.StatusStalled
	}))
	spawnsA, _, _ := jobSpawns(fl, "a")
	spawnsB, _, _ := jobSpawns(fl, "b")
	spawnsD, _, _ := jobSpawns(fl, "d")

	code, body := postRestart(t, url, id, nil)
	require.Equal(t, http.StatusOK, code, string(body))

	p, err := pips.Get(id)
	require.NoError(t, err)
	require.Equal(t, pipeline.JobDone, p.Job("a").Status)
	require.Equal(t, "A-HANDOFF", p.Job("a").Output)
	n, _, _ := jobSpawns(fl, "a")
	require.Equal(t, spawnsA, n, "done job must not be re-run")

	// b and d spawn (deps satisfied by done a); b continues from its kept branch.
	nb, promptB, baseB := jobSpawns(fl, "b")
	require.Equal(t, spawnsB+1, nb)
	require.Equal(t, branchB, baseB, "kept branch is the worktree base")
	require.Contains(t, promptB, "A-HANDOFF")
	require.Contains(t, promptB, "## Restart context")
	require.Contains(t, promptB, "previous_branch="+branchB)
	nd, promptD, _ := jobSpawns(fl, "d")
	require.Equal(t, spawnsD+1, nd)
	require.Contains(t, promptD, "A-HANDOFF")
	require.NotContains(t, promptD, "previous_branch=")
	require.Equal(t, pipeline.JobRunning, p.Job("b").Status)
	require.Equal(t, pipeline.JobPending, p.Job("c").Status, "c waits on b")
	require.Empty(t, p.Job("b").RestartBranch, "consumed by the spawn")
	require.Equal(t, pipeline.StatusRunning, p.Status)

	// The failed job's old agent record was removed (new agent reuses the id).
	require.Contains(t, fl.removedWTs, branchB)

	// Plan progress follows job state; the sealed execution is reopened.
	pl, err := plans.Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "done", pl.TaskProgress["a"])
	require.Equal(t, "in_progress", pl.TaskProgress["b"])
	require.Equal(t, "pending", pl.TaskProgress["c"])
	require.Equal(t, planstore.ExecutionStatusRunning, pl.ActiveExecution.TerminalStatus)

	// Restart context persisted under the plan-scoped key.
	rc, err := autopilot.LoadRestartContext(ctxLedgerStore{cs: srv.cstore}, autopilot.PlanRestartContextKey(id))
	require.NoError(t, err)
	require.Equal(t, 1, rc.RestartCount)
	require.Equal(t, autopilot.RestartReasonNeedsAttention, rc.ReasonKind)
}

// TestRestartPlanPipelineAfterStop: `plan stop` cancels the pipeline (jobs →
// skipped, agents killed); restart reopens it and spawns fresh agents.
func TestRestartPlanPipelineAfterStop(t *testing.T) {
	url, srv, plans, pips, root, id := newPipelineRestartPlan(t)
	ctx := context.Background()
	fl := srv.life.(*fakeLife)

	// a is running (spawned by the run). Stop the plan.
	_, err := srv.CancelPipeline(ctx, oapi.CancelPipelineRequestObject{Pid: id})
	require.NoError(t, err)
	p, _ := pips.Get(id)
	require.Equal(t, pipeline.StatusCanceled, p.Status)
	// Seed the stale agent record the cancel left behind.
	old, err := srv.store.Get(ctx, id+"-a")
	require.NoError(t, err)
	oldTmux := old.TmuxSession
	_ = root

	// Restart without force is allowed for a canceled pipeline.
	code, body := postRestart(t, url, id, nil)
	require.Equal(t, http.StatusOK, code, string(body))

	p, _ = pips.Get(id)
	require.Equal(t, pipeline.StatusRunning, p.Status)
	require.Equal(t, pipeline.JobRunning, p.Job("a").Status)
	n, prompt, _ := jobSpawns(fl, "a")
	require.Equal(t, 2, n, "a re-spawned with a new agent")
	require.Contains(t, prompt, "## Restart context")
	require.Contains(t, prompt, autopilot.RestartReasonOperatorStop)
	require.Equal(t, 1, strings.Count(prompt, "## Restart context"))
	require.Contains(t, fl.removedWTs, id+"-a")
	_ = oldTmux

	pl, _ := plans.Get(ctx, id)
	require.Equal(t, planstore.ExecutionStatusRunning, pl.ActiveExecution.TerminalStatus)

	// A second restart replaces (not stacks) the section; running agent needs force.
	code, body = postRestart(t, url, id, nil)
	require.Equal(t, http.StatusConflict, code, string(body))
	require.Contains(t, string(body), "pass --force")
	code, body = postRestart(t, url, id, map[string]any{"force": true})
	require.Equal(t, http.StatusOK, code, string(body))
	_, prompt, _ = jobSpawns(fl, "a")
	require.Equal(t, 1, strings.Count(prompt, "## Restart context"))
	require.Contains(t, prompt, "restart #2")
}
