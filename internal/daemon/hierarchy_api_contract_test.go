package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/srjn45/warden/internal/ctxstore"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/projectstore"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// TestHierarchyFieldsAreExposedOnReadSurfaces locks the wire contract for the
// persisted hierarchy edges. The read endpoints deliberately return the stored
// entities rather than recomputing membership, so a dangling id remains visible
// to clients that need to render or repair the complete hierarchy.
func TestHierarchyFieldsAreExposedOnReadSurfaces(t *testing.T) {
	fs := newFakeStore()
	ps, err := pipeline.NewStore(t.TempDir())
	require.NoError(t, err)
	cs, err := ctxstore.New(t.TempDir())
	require.NoError(t, err)
	projects, err := projectstore.NewStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { projects.Close() })

	project, err := projects.OpenProject("/projects/alpha", "alpha", "/projects/alpha")
	require.NoError(t, err)
	project.Agents = []string{"agent-parent", "agent-child"}
	project.Pipelines = []string{"pipeline-child"}
	project.Terminals = []string{"terminal-1"}
	require.NoError(t, projects.Upsert(project))

	fs.data["agent-child"] = &store.Session{
		ID: "agent-child", Status: store.StatusWorking, ProjectID: project.ID,
		ParentID: "agent-parent", ChildAgents: []string{"agent-grandchild"},
		ChildPipelines: []string{"pipeline-child"},
	}
	require.NoError(t, ps.Create(&pipeline.Pipeline{
		ID: "pipeline-child", Name: "pipeline-child", Repo: project.Path,
		ProjectID: project.ID, ParentAgentID: "agent-child",
	}))

	exec := NewExecutor(ps, fs, &fakeLife{}, cs, func() {})
	srv := &Server{store: fs, life: &fakeLife{}, exec: exec, projects: projects, hub: newHub(), done: make(chan struct{})}
	ts := httptest.NewServer(srv.router())
	defer ts.Close()

	getObject := func(path string) map[string]any {
		t.Helper()
		resp, err := http.Get(ts.URL + path)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		var body map[string]any
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
		return body
	}

	sessions := getObject("/api/v1/sessions")
	session := sessions["sessions"].([]any)[0].(map[string]any)
	require.Equal(t, "agent-parent", session["parent_id"])
	require.Equal(t, []any{"agent-grandchild"}, session["child_agents"])
	require.Equal(t, []any{"pipeline-child"}, session["child_pipelines"])
	require.Equal(t, project.ID, session["project_id"])

	pipelines := getObject("/api/v1/pipelines")
	pipelineBody := pipelines["pipelines"].([]any)[0].(map[string]any)
	require.Equal(t, project.ID, pipelineBody["project_id"])
	require.Equal(t, "agent-child", pipelineBody["parent_agent_id"])

	projectBody := getObject("/api/v1/projects")["projects"].([]any)[0].(map[string]any)
	require.Equal(t, []any{"agent-parent", "agent-child"}, projectBody["agents"])
	require.Equal(t, []any{"pipeline-child"}, projectBody["pipelines"])
	require.Equal(t, []any{"terminal-1"}, projectBody["terminals"])

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := newStreamRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/events/stream", nil).WithContext(ctx)
	go srv.handleEventsStream(rec, req)
	var frame struct {
		Sessions []map[string]any `json:"sessions"`
	}
	require.NoError(t, json.Unmarshal([]byte(readEvent(t, bufio.NewReader(rec.reader()))), &frame))
	require.Len(t, frame.Sessions, 1)
	require.Equal(t, "agent-parent", frame.Sessions[0]["parent_id"])
	require.Equal(t, []any{"agent-grandchild"}, frame.Sessions[0]["child_agents"])
	require.Equal(t, []any{"pipeline-child"}, frame.Sessions[0]["child_pipelines"])
	require.Equal(t, project.ID, frame.Sessions[0]["project_id"])
}
