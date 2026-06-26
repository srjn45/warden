package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

// connectTo wires an in-memory MCP client to a server bound to the given daemon
// URL and returns a live session (closed by the test via t.Cleanup).
func connectTo(t *testing.T, daemonURL string) *mcpsdk.ClientSession {
	t.Helper()
	srv := NewServer(daemonURL)
	ctx := context.Background()
	clientTransport, serverTransport := mcpsdk.NewInMemoryTransports()
	go func() { _ = srv.Run(ctx, serverTransport) }()
	cl := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "0"}, nil)
	session, err := cl.Connect(ctx, clientTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// TestExtraToolsRegistered asserts every parity tool is advertised by the server.
func TestExtraToolsRegistered(t *testing.T) {
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer daemon.Close()
	session := connectTo(t, daemon.URL)

	got := map[string]bool{}
	for tool, err := range session.Tools(context.Background(), nil) {
		require.NoError(t, err)
		got[tool.Name] = true
	}

	want := []string{
		"digest", "get_metrics", "savings", "search", "history", "audit_log",
		"list_worktrees", "list_plugins", "get_pressure",
		"set_auto_approve", "set_permission_mode", "prune_worktrees",
		"export_sessions", "import_sessions", "rotate_agent", "handoff_agent",
		"pause_pipeline", "resume_pipeline", "retry_pipeline_job",
		"edit_pipeline_job", "emit_pipeline_output", "delete_pipeline",
		"validate_pipeline", "list_pipeline_templates", "library_list",
		"create_schedule", "delete_schedule",
	}
	for _, name := range want {
		require.Truef(t, got[name], "tool %q should be registered", name)
	}
}

// TestValidatePipelineTool runs entirely locally (no daemon contact).
func TestValidatePipelineTool(t *testing.T) {
	session := connectTo(t, "http://127.0.0.1:0")
	ctx := context.Background()

	res, err := session.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "validate_pipeline",
		Arguments: map[string]any{"spec": "name: demo\nrepo: /tmp/demo\njobs:\n  - id: a\n    prompt: do a\n"},
	})
	require.NoError(t, err)
	require.False(t, res.IsError, textOf(res))
	require.Contains(t, textOf(res), `"valid": true`)
	require.Contains(t, textOf(res), `"id": "demo"`)

	bad, err := session.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "validate_pipeline",
		Arguments: map[string]any{"spec": "not: [valid"},
	})
	require.NoError(t, err)
	require.Contains(t, textOf(bad), "invalid pipeline")
}

// TestListPipelineTemplatesTool also needs no daemon.
func TestListPipelineTemplatesTool(t *testing.T) {
	session := connectTo(t, "http://127.0.0.1:0")
	res, err := session.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "list_pipeline_templates"})
	require.NoError(t, err)
	require.False(t, res.IsError, textOf(res))
}

// TestLibraryListTool returns both presets and templates without a daemon.
func TestLibraryListTool(t *testing.T) {
	session := connectTo(t, "http://127.0.0.1:0")
	res, err := session.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "library_list"})
	require.NoError(t, err)
	require.False(t, res.IsError, textOf(res))
	require.Contains(t, textOf(res), `"presets"`)
	require.Contains(t, textOf(res), `"prompt_templates"`)
	require.Contains(t, textOf(res), `"templates"`)
}

// TestSavingsTool exercises the GET /savings round-trip.
func TestSavingsTool(t *testing.T) {
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/savings", r.URL.Path)
		_, _ = w.Write([]byte(`{"agents":3,"saved_tokens":42000}`))
	}))
	defer daemon.Close()
	session := connectTo(t, daemon.URL)

	res, err := session.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "savings"})
	require.NoError(t, err)
	require.False(t, res.IsError, textOf(res))
	require.Contains(t, textOf(res), "42000")
}

// TestPausePipelineTool exercises a pipeline write verb.
func TestPausePipelineTool(t *testing.T) {
	var hit string
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = r.Method + " " + r.URL.Path
		_, _ = w.Write([]byte(`{}`))
	}))
	defer daemon.Close()
	session := connectTo(t, daemon.URL)

	res, err := session.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name:      "pause_pipeline",
		Arguments: map[string]any{"pipeline": "ship-it"},
	})
	require.NoError(t, err)
	require.False(t, res.IsError, textOf(res))
	require.Equal(t, "POST /api/v1/pipelines/ship-it/pause", hit)
	require.Contains(t, textOf(res), "paused pipeline ship-it")
}

// TestSetAutoApproveTool exercises a PATCH lifecycle control.
func TestSetAutoApproveTool(t *testing.T) {
	var hit string
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = r.Method + " " + r.URL.Path
		_, _ = w.Write([]byte(`{}`))
	}))
	defer daemon.Close()
	session := connectTo(t, daemon.URL)

	res, err := session.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name:      "set_auto_approve",
		Arguments: map[string]any{"ticket": "A-1", "enabled": true},
	})
	require.NoError(t, err)
	require.False(t, res.IsError, textOf(res))
	require.Equal(t, "PATCH /api/v1/sessions/A-1/auto-approve", hit)
	require.Contains(t, textOf(res), "auto-approve on for A-1")
}

// TestHandoffAgentRetire asserts handoff_agent{retire:true} runs the rotate path:
// it spawns a successor in the ticket agent's worktree, then reaps that agent —
// identical to rotate_agent.
func TestHandoffAgentRetire(t *testing.T) {
	var hits []string
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.Method+" "+r.URL.Path)
		switch {
		case r.URL.Path == "/api/v1/sessions/OLD-1" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"id":"OLD-1","workdir":"/repo/.worktrees/OLD-1","permission_mode":"acceptEdits"}`))
		case r.URL.Path == "/api/v1/spawn":
			_, _ = w.Write([]byte(`{"id":"SUCC-1","workdir":"/repo/.worktrees/OLD-1"}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer daemon.Close()
	session := connectTo(t, daemon.URL)

	res, err := session.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name:      "handoff_agent",
		Arguments: map[string]any{"retire": true, "ticket": "OLD-1", "prompt": "carry on"},
	})
	require.NoError(t, err)
	require.False(t, res.IsError, textOf(res))
	require.Contains(t, textOf(res), `"successor": "SUCC-1"`)
	require.Contains(t, textOf(res), `"retired": "OLD-1"`)
	require.Contains(t, hits, "POST /api/v1/sessions/OLD-1/terminate", "retire must reap the ticket agent")
}

// TestHandoffAgentRetireRejectsTo guards the mutual exclusion at the MCP surface.
func TestHandoffAgentRetireRejectsTo(t *testing.T) {
	session := connectTo(t, "http://127.0.0.1:0")
	res, err := session.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name:      "handoff_agent",
		Arguments: map[string]any{"retire": true, "to": "B-2", "prompt": "x", "ticket": "OLD-1"},
	})
	require.NoError(t, err)
	require.Contains(t, textOf(res), "mutually exclusive")
}

// TestCreateScheduleTool exercises the schedule create round-trip.
func TestCreateScheduleTool(t *testing.T) {
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "POST /api/v1/schedules", r.Method+" "+r.URL.Path)
		_, _ = w.Write([]byte(`{"id":"nightly","cron":"@daily"}`))
	}))
	defer daemon.Close()
	session := connectTo(t, daemon.URL)

	res, err := session.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name:      "create_schedule",
		Arguments: map[string]any{"name": "nightly", "cron": "@daily", "prompt": "tidy up"},
	})
	require.NoError(t, err)
	require.False(t, res.IsError, textOf(res))
	require.Contains(t, textOf(res), "nightly")
}
