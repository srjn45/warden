package schema3

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/planstore"
	"github.com/srjn45/warden/internal/store"
	"github.com/srjn45/warden/internal/terminalstore"
	"github.com/stretchr/testify/require"
)

// Populate production wire types rather than a reduced migration header. This
// deliberately exercises nested permissions, recovery/runtime, digest, event,
// quota and model fields that relationship-only codecs used to discard.
func populate(v reflect.Value, depth int) {
	if depth > 9 || !v.CanSet() {
		return
	}
	if v.Type() == reflect.TypeFor[time.Time]() {
		v.Set(reflect.ValueOf(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)))
		return
	}
	switch v.Kind() {
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		populate(v.Elem(), depth+1)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			populate(v.Field(i), depth+1)
		}
	case reflect.String:
		v.SetString("business-payload")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(7)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(7)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(7)
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		populate(v.Index(0), depth+1)
	case reflect.Map:
		if v.Type().Key().Kind() == reflect.String {
			v.Set(reflect.MakeMap(v.Type()))
			key := reflect.New(v.Type().Key()).Elem()
			key.SetString("business-key")
			value := reflect.New(v.Type().Elem()).Elem()
			populate(value, depth+1)
			v.SetMapIndex(key, value)
		}
	}
}

func TestActualSchema2WholePipelineAndPlanPayloads(t *testing.T) {
	g := fixture()
	p := pipeline.Pipeline{
		ID: "pipe", Name: "business-pipeline", Repo: "/fixture/repo", ProjectID: "p", PlanID: "plan",
		Jobs: []pipeline.Job{
			{ID: "z", Prompt: "first declaration", Status: pipeline.JobDone, Output: "retain result", Type: "code", AgentID: "worker"},
			{ID: "a", Prompt: "second declaration", DependsOn: []string{"z"}, Status: pipeline.JobPending},
			{ID: "join", Type: "span-in", Status: pipeline.JobDone},
		},
	}
	plan := planstore.Plan{
		ID: "plan", ProjectID: "p", Name: "business-plan", Goal: "retain complete definition", Revision: 17,
		Tasks:        []planstore.PlanTask{{ID: "z", Prompt: "first declaration"}, {ID: "a", Prompt: "second declaration", After: []string{"z"}}},
		TaskProgress: map[string]string{"z": "done", "a": "pending"},
		TaskOutcomes: map[string]planstore.TaskOutcome{"z": {TaskID: "z", Status: "done"}},
	}
	planstore.RefreshContentHash(&plan)
	g["pipelines"]["pipe"] = wire(t, p)
	g["plans"]["plan"] = wire(t, plan)
	g["agents"]["worker"]["pipeline_id"] = "pipe"
	g["agents"]["worker"]["job_id"] = "z"
	out, report, err := Map(g)
	require.NoError(t, err, string(encode(report.Findings)))
	require.Equal(t, plan.ContentHash, out["plans"]["plan"]["content_hash"])
	require.Equal(t, json.Number("17"), out["plans"]["plan"]["revision"])
	require.NotContains(t, out["plans"]["plan"], "tasks")
	require.NotContains(t, out["pipelines"]["pipe"], "jobs")
	require.NotContains(t, out["pipelines"]["pipe"], "job_count")
	require.NotContains(t, out["pipelines"]["pipe"], "jobs_done")
	require.Equal(t, "retain result", out["pipeline_jobs"][Tuple("pipe", "z")]["output"])
	require.Equal(t, 0, out["plan_tasks"][Tuple("plan", "z")]["seq"])
	require.Equal(t, 1, out["plan_tasks"][Tuple("plan", "a")]["seq"])
	for _, field := range []string{"job_count", "jobs_done"} {
		t.Run(field, func(t *testing.T) {
			bad := clone(g)
			bad["pipelines"]["pipe"][field] = json.Number("900")
			_, report, err := Map(bad)
			require.Error(t, err)
			require.Contains(t, string(encode(report.Findings)), "computed counter disagrees")
		})
	}
}
func wire(t *testing.T, v any) Object {
	t.Helper()
	var o Object
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	require.NoError(t, d.Decode(&o))
	return o
}
func TestActualSchema2FullAgentTerminalAndJobPayloads(t *testing.T) {
	var a agentstore.Agent
	populate(reflect.ValueOf(&a).Elem(), 0)
	a.ID = "worker"
	a.ProjectID = "p"
	a.PlanID = "plan"
	a.ParentID = ""
	a.PipelineID = "pipe"
	a.JobID = "job"
	a.AutopilotRunID = ""
	a.AutopilotTaskID = ""
	a.AutopilotSlot = ""
	a.Scope = ""
	a.Lifecycle = ""
	a.ArchivedAt = nil
	a.ClosedAt = nil
	a.ChildAgents = nil
	a.ChildPipelines = nil
	a.ChildAutopilots = nil
	a.Status = store.Status("idle")
	var terminal terminalstore.Terminal
	populate(reflect.ValueOf(&terminal).Elem(), 0)
	terminal.ID = "terminal"
	terminal.ProjectID = "p"
	var j pipeline.Job
	populate(reflect.ValueOf(&j).Elem(), 0)
	j.ID = "job"
	j.DependsOn = nil
	j.AgentID = "worker"
	j.SessionID = "worker"
	j.Status = pipeline.JobDone
	g := fixture()
	agentBody, terminalBody, jobBody := wire(t, a), wire(t, terminal), wire(t, j)
	g["agents"]["worker"] = agentBody
	g["terminals"] = map[string]Object{"terminal": terminalBody}
	g["pipelines"]["pipe"]["jobs"] = []any{jobBody}
	out, r, err := Map(g)
	require.NoError(t, err, string(encode(r.Findings)))
	for _, tc := range []struct {
		before, after Object
		removed       string
	}{{agentBody, out["agents"]["worker"], "parent_id child_agents child_pipelines child_autopilots"}, {terminalBody, out["terminals"]["terminal"], ""}, {jobBody, out["pipeline_jobs"][Tuple("pipe", "job")], "id agent_id session_id"}} {
		expected := clone(tc.before)
		remove(expected, tc.removed)
		for k, v := range expected {
			require.Equal(t, Digest(v), Digest(tc.after[k]), k)
		}
	}
}
