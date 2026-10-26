package schema3

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Long-lived schema-2 installs accumulate stale bookkeeping that carries no
// ownership. Each case must map cleanly and leave an auditable finding.

func dispositions(r *Report, c, f string) []string {
	var out []string
	for _, x := range r.Findings {
		if x.Collection == c && x.Field == f {
			out = append(out, x.Disposition)
		}
	}
	return out
}

func TestStaleForwardListsDeferToChild(t *testing.T) {
	g := fixture()
	g["projects"]["p"]["agents"] = []any{"absent"} // misses worker, names a ghost
	out, r, err := Map(g)
	require.NoError(t, err)
	require.Equal(t, "p", out["agents"]["worker"]["project_id"])
	require.Contains(t, dispositions(r, "projects", "agents"), "derived from child")
	require.Contains(t, dispositions(r, "projects", "agents"), "dropped missing child")
}

func TestArchivedAgentWithoutProjectIsDeleted(t *testing.T) {
	g := fixture()
	g["archived_agents"]["orphan"] = Object{"id": "orphan", "status": "done"}
	g["archived_agents"]["past"]["parent_id"] = "orphan"
	out, r, err := Map(g)
	require.NoError(t, err)
	require.NotContains(t, out["agents"], "orphan")
	require.NotContains(t, out["agents"]["past"], "parent_agent_id")
	require.Contains(t, dispositions(r, "agents", "project_id"), "deleted archived without project")
	require.Contains(t, dispositions(r, "agents", "parent_agent_id"), "cleared deleted parent")
}

func TestLiveAgentWithoutProjectStillBlocks(t *testing.T) {
	g := fixture()
	g["agents"]["loose"] = Object{"id": "loose", "status": "idle"}
	out, r, err := Map(g)
	require.Error(t, err)
	require.Nil(t, out)
	require.Contains(t, dispositions(r, "agents", "project_id"), "blocked")
}

func TestDanglingAutopilotBindingsAreCleared(t *testing.T) {
	g := fixture()
	g["archived_agents"]["past"]["autopilot_run_id"] = "ap-gone"
	g["archived_agents"]["past"]["autopilot_slot"] = "autopilot"
	g["archived_agents"]["past"]["autopilot_task_id"] = "t1"
	g["agents"]["worker"]["autopilot_task_id"] = "t2" // no run at all
	out, r, err := Map(g)
	require.NoError(t, err)
	for _, f := range []string{"autopilot_run_id", "autopilot_slot", "autopilot_task_id"} {
		require.NotContains(t, out["agents"]["past"], f)
	}
	require.NotContains(t, out["agents"]["worker"], "autopilot_task_id")
	require.Contains(t, dispositions(r, "agents", "autopilot_run_id"), "cleared missing run")
	require.Contains(t, dispositions(r, "agents", "autopilot_task_id"), "cleared task without run")
}

func TestOrphanReservedContextIsDropped(t *testing.T) {
	g := fixture()
	g["context_entries"] = map[string]Object{"autopilot.ap-gone.tasks": {"key": "autopilot.ap-gone.tasks", "value": "[]"}}
	out, r, err := Map(g)
	require.NoError(t, err)
	require.NotContains(t, out["context_entries"], "autopilot.ap-gone.tasks")
	require.Contains(t, dispositions(r, "context_entries", "key"), "dropped orphan reserved run")
}

func TestFinishedPlanKeepsHistoryOfMissingExecutor(t *testing.T) {
	for status, ok := range map[string]bool{"completed": true, "archived": true, "in_progress": false} {
		t.Run(status, func(t *testing.T) {
			g := fixture()
			p := g["plans"]["plan"]
			p["status"] = status
			p["autopilot_run_id"] = "ap-gone"
			p["active_execution"] = Object{"execution_mode": "autopilot", "executor_id": "ap-gone"}
			out, r, err := Map(g)
			require.Contains(t, dispositions(r, "plans", "autopilot_run_id"), "dropped missing executor mirror")
			if !ok {
				require.Error(t, err)
				require.Contains(t, dispositions(r, "plans", "active_execution.executor_id"), "blocked")
				return
			}
			require.NoError(t, err)
			require.Equal(t, "ap-gone", object(out["plans"]["plan"]["active_execution"])["executor_id"])
			require.Contains(t, dispositions(r, "plans", "active_execution.executor_id"), "executor gone (finished plan)")
		})
	}
}

func TestMissingContentHashIsDerived(t *testing.T) {
	g := fixture()
	want := g["plans"]["plan"]["content_hash"]
	delete(g["plans"]["plan"], "content_hash")
	out, r, err := Map(g)
	require.NoError(t, err)
	require.Equal(t, want, out["plans"]["plan"]["content_hash"])
	require.Contains(t, dispositions(r, "plans", "content_hash"), "initialized")
}
