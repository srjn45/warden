package orchestrator

import (
	"encoding/json"
	"testing"

	"github.com/srjn45/warden/internal/client"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}

// list_agents becomes a headed table, not a wall of JSON.
func TestPresent_ListAgentsTable(t *testing.T) {
	raw := mustJSON(t, []*store.Session{
		{ID: "agent-1", Type: store.TypeDevelopment, Status: store.StatusWorking, Subject: "fix the timeout bug"},
		{ID: "agent-2", Type: store.TypeAnalysis, Status: store.StatusDone, Name: "fe-review"},
	})
	out := present("list_agents", raw)
	require.Contains(t, out, "2 agents")
	require.Contains(t, out, "ID")
	require.Contains(t, out, "STATUS")
	require.Contains(t, out, "agent-1")
	require.Contains(t, out, "fix the timeout bug")
	require.Contains(t, out, "fe-review")
	require.NotContains(t, out, "claude_session_id", "raw JSON fields are gone")
	require.NotContains(t, out, `{"id"`, "no JSON braces leak through")
}

func TestPresent_ListAgentsEmpty(t *testing.T) {
	require.Equal(t, "no agents", present("list_agents", "[]"))
}

// A multi-line prompt is flattened into one table cell.
func TestPresent_ListAgentsFlattensPrompt(t *testing.T) {
	raw := mustJSON(t, []*store.Session{{ID: "a1", Status: store.StatusWorking,
		Prompt: "line one\nline two\nline three"}})
	out := present("list_agents", raw)
	require.Contains(t, out, "line one line two")
	require.NotContains(t, out, "line one\nline two", "newlines collapsed inside the cell")
}

// get_agent becomes a labelled detail block; empty fields are dropped.
func TestPresent_GetAgentDetail(t *testing.T) {
	raw := mustJSON(t, store.Session{ID: "agent-1", Type: store.TypeDevelopment,
		Status: store.StatusWorking, Repo: "/home/u/warden", Branch: "feat/x", Subject: "do the thing"})
	out := present("get_agent", raw)
	require.Contains(t, out, "id")
	require.Contains(t, out, "agent-1")
	require.Contains(t, out, "/home/u/warden")
	require.Contains(t, out, "do the thing")
	require.NotContains(t, out, "pr", "empty fields like pr are omitted")
}

func TestPresent_CtxListTable(t *testing.T) {
	raw := mustJSON(t, []client.ContextEntry{{Key: "build.status", Value: "green", UpdatedBy: "agent-1"}})
	out := present("ctx_list", raw)
	require.Contains(t, out, "KEY")
	require.Contains(t, out, "build.status")
	require.Contains(t, out, "green")
}

func TestPresent_PipelineListTable(t *testing.T) {
	raw := mustJSON(t, []*pipeline.Pipeline{{ID: "p1", Repo: "/r", Jobs: []pipeline.Job{{ID: "j1"}, {ID: "j2"}}}})
	out := present("pipeline_list", raw)
	require.Contains(t, out, "ID")
	require.Contains(t, out, "p1")
	require.Contains(t, out, "2", "job count shown")
}

// A scalar result (a context value, an id, "spawned …") passes through untouched.
func TestPresent_ScalarPassThrough(t *testing.T) {
	require.Equal(t, "spawned agent-9", present("spawn_agent", "spawned agent-9"))
	require.Equal(t, "green", present("ctx_get", "green"))
}

// An unrecognised tool whose payload is JSON gets indented rather than dumped on
// one line.
func TestPresent_UnknownToolIndentsJSON(t *testing.T) {
	out := present("some_future_tool", `{"a":1,"b":{"c":2}}`)
	require.Contains(t, out, "\n", "indented across lines")
	require.Contains(t, out, "  ", "two-space indent")
}

// Malformed JSON for a known tool never panics — it falls back to the raw text.
func TestPresent_MalformedFallsBack(t *testing.T) {
	require.Equal(t, "not json {", present("list_agents", "not json {"))
}
