package schema3

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func autopilotFixture() Generation {
	g := fixture()
	p := g["plans"]["plan"]
	p["active_execution"] = Object{"id": "execution", "plan_id": "plan", "execution_mode": "autopilot", "executor_id": "live", "task_progress": Object{"z": "done", "a": "in_progress"}, "snapshot": Object{"tasks": []any{Object{"id": "original", "prompt": "snapshot"}}}}
	p["autopilot_run_id"] = "live"
	g["autopilot_runs"] = map[string]Object{"run": {"run_id": "run", "project_id": "p", "plan_id": "plan", "name": "registration", "state": "active", "brain_id": "manager", "integration_branch": "integration", "created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-02T00:00:00Z"}}
	g["autopilots"] = map[string]Object{"live": {"id": "live", "project_id": "p", "plan_id": "plan", "name": "AP:ordered", "manager_agent_id": "manager", "brain_agent_id": "brain", "diagnostics": Object{"state": "active", "integration_branch": "integration", "last_error": "diagnostic"}, "created_at": "2025-12-01T00:00:00Z", "updated_at": "2026-02-01T00:00:00Z"}}
	g["agents"]["manager"] = Object{"id": "manager", "project_id": "p", "plan_id": "plan", "autopilot_run_id": "live", "autopilot_slot": "autopilot", "role": "autopilot"}
	g["agents"]["brain"] = Object{"id": "brain", "project_id": "p", "plan_id": "plan", "role": "brain"}
	g["context_entries"] = map[string]Object{}
	add := func(key, value string) {
		g["context_entries"][key] = Object{"key": key, "value": value, "updated_by": "daemon"}
	}
	add("autopilot.run.tasks", `[{"id":"a","state":"in_progress","branch":"work","worker_id":"worker","pr":7,"note":"review"}]`)
	add("autopilot.run.tasks.a.state", "in_progress")
	add("autopilot.run.tasks.a.branch", "work")
	add("autopilot.run.integration_branch", `"integration"`)
	add("autopilot.run.landings", `[{"branch":"previous","sha":"abc","pr":1}]`)
	add("autopilot.run.fix.a", `{"last_dispatched_sha":"head","fix_attempts":2}`)
	add("autopilot.run.restart_context", `{"facts":["keep"]}`)
	add("ordinary", "collaboration")
	return g
}
func TestMappingAutopilotSoleAuthorityAndDispatch(t *testing.T) {
	g := autopilotFixture()
	out, r, err := Map(g)
	require.NoError(t, err, string(encode(r.Findings)))
	require.Equal(t, "run", r.Executors["live"])
	require.NotContains(t, out, "autopilots")
	run := out["autopilot_runs"]["run"]
	require.NotContains(t, run, "brain_id")
	require.NotContains(t, run, "diagnostics")
	require.Equal(t, "registration", run["name"])
	require.Equal(t, "diagnostic", run["last_error"])
	require.Equal(t, "2025-12-01T00:00:00Z", run["created_at"])
	require.Equal(t, "2026-02-01T00:00:00Z", run["updated_at"])
	require.Equal(t, "run", out["agents"]["manager"]["autopilot_run_id"])
	require.Equal(t, "autopilot", out["agents"]["manager"]["autopilot_slot"])
	require.Equal(t, "brain", out["agents"]["brain"]["autopilot_slot"])
	active := object(out["plans"]["plan"]["active_execution"])
	require.Equal(t, "run", active["executor_id"])
	require.NotContains(t, active, "task_progress")
	require.Equal(t, object(g["plans"]["plan"]["active_execution"])["snapshot"], active["snapshot"])
	row := out["plan_tasks"][Tuple("plan", "a")]
	require.Equal(t, "in_progress", row["status"])
	require.Equal(t, "execution", object(row["dispatch"])["execution_id"])
	for _, kind := range []string{"tasks", "tasks.a.state", "tasks.a.branch", "integration_branch"} {
		require.NotContains(t, out["context_entries"], "autopilot.run."+kind)
	}
	for _, kind := range []string{"landings", "fix.a", "restart_context"} {
		require.Equal(t, g["context_entries"]["autopilot.run."+kind], out["context_entries"]["autopilot.run."+kind])
	}
	require.Contains(t, out["context_entries"], "ordinary")
}
func TestMappingAutopilotConflicts(t *testing.T) {
	for name, mutate := range map[string]func(Generation){
		"terminal live pair": func(g Generation) { g["autopilot_runs"]["run"]["state"] = "complete" },
		"branch conflict":    func(g Generation) { object(g["autopilots"]["live"]["diagnostics"])["integration_branch"] = "different" },
		"unknown state":      func(g Generation) { g["autopilot_runs"]["run"]["state"] = "running" },
		"unknown diagnostic": func(g Generation) { object(g["autopilots"]["live"]["diagnostics"])["task_ledger"] = []any{} },
		"overlay conflict":   func(g Generation) { g["context_entries"]["autopilot.run.tasks.a.state"]["value"] = "landed" },
		"unproven landing": func(g Generation) {
			g["context_entries"]["autopilot.run.tasks"]["value"] = `[{"id":"a","state":"landed","branch":"unknown"}]`
		},
		"unknown context": func(g Generation) { g["context_entries"]["autopilot.run.unknown"] = Object{"value": "unknown"} },
		"unknown task": func(g Generation) {
			g["context_entries"]["autopilot.run.tasks"]["value"] = `[{"id":"missing","state":"pending"}]`
		},
		"active progress conflict": func(g Generation) {
			object(object(g["plans"]["plan"]["active_execution"])["task_progress"])["a"] = "done"
		},
		"two managers": func(g Generation) {
			g["agents"]["other"] = clone(g["agents"]["manager"])
			g["agents"]["other"]["id"] = "other"
		},
		"unpaired no selection": func(g Generation) {
			delete(g, "autopilot_runs")
			object(g["plans"]["plan"]["active_execution"])["executor_id"] = "missing"
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := autopilotFixture()
			mutate(g)
			out, r, err := Map(g)
			require.Error(t, err)
			require.Nil(t, out)
			require.True(t, r.Blocked())
		})
	}
}
func TestMappingHistoricalPlanlessAndUnpairedLive(t *testing.T) {
	g := fixture()
	g["autopilot_runs"] = map[string]Object{"historical": {"run_id": "historical", "project_id": "p", "state": "complete"}, "dormant": {"run_id": "dormant", "project_id": "p", "state": "registered"}}
	out, _, err := Map(g)
	require.NoError(t, err)
	require.Len(t, out["autopilot_runs"], 2)
	g = autopilotFixture()
	delete(g["autopilot_runs"], "run")
	delete(g, "context_entries")
	g["agents"]["manager"]["autopilot_run_id"] = "live"
	out, _, err = Map(g)
	require.NoError(t, err)
	require.Contains(t, out["autopilot_runs"], "live")
}
func TestMappingMultipleWorkersAreNotManagerSlotConflict(t *testing.T) {
	g := autopilotFixture()
	for _, id := range []string{"one", "two"} {
		g["agents"][id] = Object{"id": id, "project_id": "p", "autopilot_run_id": "run", "autopilot_slot": "worker"}
	}
	_, report, err := Map(g)
	require.NoError(t, err, string(encode(report.Findings)))
}

func TestMappingHistoricalDispatchBecomesPlanEvidence(t *testing.T) {
	g := fixture()
	g["autopilot_runs"] = map[string]Object{"history": {"run_id": "history", "project_id": "p", "plan_id": "plan", "state": "complete"}}
	p := g["plans"]["plan"]
	p["execution_history"] = []any{Object{"id": "past-execution", "execution_mode": "autopilot", "executor_id": "history", "snapshot": Object{"tasks": []any{Object{"id": "old-task", "prompt": "old definition"}}}}}
	g["context_entries"] = map[string]Object{"autopilot.history.tasks": {"value": `[{"id":"old-task","state":"landed","branch":"past","pr":1,"worker_id":"deleted-attribution"}]`}, "autopilot.history.tasks.old-task.state": {"value": "landed"}, "autopilot.history.landings": {"value": `[{"branch":"past","sha":"merge-sha","pr":1}]`}}
	out, r, err := Map(g)
	require.NoError(t, err, string(encode(r.Findings)))
	history := object(list(out["plans"]["plan"]["execution_history"])[0])
	require.Equal(t, "history", history["executor_id"])
	require.Equal(t, "landed", object(object(history["task_dispatch"])["old-task"])["state"])
	require.NotContains(t, out["plan_tasks"], Tuple("plan", "old-task"))
	require.Equal(t, object(list(p["execution_history"])[0])["snapshot"], history["snapshot"])
}
