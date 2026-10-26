package schema3

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/pipeline"
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
