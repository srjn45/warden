package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

const samplePlanJSON = `{"id":"plan-ab12cd34","project_id":"/tmp/proj","name":"feature-x",
	"goal":"ship the feature","file_path":"plans/pending/feature-x.yaml","status":"pending",
	"constraints":["stay in lane"],"done_when":["tests pass"],
	"tasks":[{"id":"t1","prompt":"do the work"}],
	"task_progress":{"t1":"pending"},
	"created_at":"2026-09-28T00:00:00Z","updated_at":"2026-09-28T00:00:00Z"}`

// TestPlanToolsRegistered asserts every plan tool is advertised by the server.
func TestPlanToolsRegistered(t *testing.T) {
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
		"list_plans", "get_plan", "create_plan", "update_plan",
		"scan_plans", "update_plan_status", "archive_plan", "assess_plan",
		"run_plan", "complete_plan", "update_task_status",
	}
	for _, name := range want {
		require.Truef(t, got[name], "tool %q should be registered", name)
	}
}

func TestListPlansTool(t *testing.T) {
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/plans" && r.Method == http.MethodGet {
			require.Equal(t, "/tmp/proj", r.URL.Query().Get("project_id"))
			_, _ = w.Write([]byte("[" + samplePlanJSON + "]"))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer daemon.Close()
	session := connectTo(t, daemon.URL)

	res, err := session.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name:      "list_plans",
		Arguments: map[string]any{"project_id": "/tmp/proj"},
	})
	require.NoError(t, err)
	require.False(t, res.IsError, textOf(res))
	require.Contains(t, textOf(res), `"plan-ab12cd34"`)
	require.Contains(t, textOf(res), `"feature-x"`)
}

func TestGetPlanTool(t *testing.T) {
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/plans/plan-ab12cd34" && r.Method == http.MethodGet {
			_, _ = w.Write([]byte(samplePlanJSON))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer daemon.Close()
	session := connectTo(t, daemon.URL)

	res, err := session.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name:      "get_plan",
		Arguments: map[string]any{"plan_id": "plan-ab12cd34"},
	})
	require.NoError(t, err)
	require.False(t, res.IsError, textOf(res))
	require.Contains(t, textOf(res), `"plan-ab12cd34"`)
	require.Contains(t, textOf(res), `"pending"`)
	require.Contains(t, textOf(res), `"ship the feature"`)
}

func TestCreatePlanTool(t *testing.T) {
	var body string
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/plans" && r.Method == http.MethodPost {
			b, _ := io.ReadAll(r.Body)
			body = string(b)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(samplePlanJSON))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer daemon.Close()
	session := connectTo(t, daemon.URL)

	res, err := session.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name: "create_plan",
		Arguments: map[string]any{
			"project_id":  "/tmp/proj",
			"name":        "feature-x",
			"goal":        "ship the feature",
			"tasks":       []map[string]any{{"id": "t1", "prompt": "do the work"}},
			"constraints": []string{"stay in lane"},
			"done_when":   []string{"tests pass"},
		},
	})
	require.NoError(t, err)
	require.False(t, res.IsError, textOf(res))
	require.Contains(t, textOf(res), `"plan-ab12cd34"`)
	require.Contains(t, body, `"goal":"ship the feature"`)
	require.Contains(t, body, `"do the work"`)
}

func TestUpdatePlanTool(t *testing.T) {
	var method, path, body string
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/api/v1/plans/plan-ab12cd34") && r.Method == http.MethodPatch {
			b, _ := io.ReadAll(r.Body)
			method, path, body = r.Method, r.URL.Path, string(b)
			_, _ = w.Write([]byte(samplePlanJSON))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer daemon.Close()
	session := connectTo(t, daemon.URL)

	res, err := session.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name:      "update_plan",
		Arguments: map[string]any{"plan_id": "plan-ab12cd34", "goal": "ship better"},
	})
	require.NoError(t, err)
	require.False(t, res.IsError, textOf(res))
	require.Equal(t, http.MethodPatch, method)
	require.Equal(t, "/api/v1/plans/plan-ab12cd34", path)
	require.Contains(t, body, `"goal":"ship better"`)
	require.Contains(t, textOf(res), `"feature-x"`)
}

func TestScanPlansTool(t *testing.T) {
	var method string
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/plans/scan") {
			method = r.Method
			_, _ = w.Write([]byte(`{"upserted":2}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer daemon.Close()
	session := connectTo(t, daemon.URL)

	res, err := session.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name:      "scan_plans",
		Arguments: map[string]any{"project_id": "/tmp/proj"},
	})
	require.NoError(t, err)
	require.False(t, res.IsError, textOf(res))
	require.Contains(t, textOf(res), `"upserted"`)
	require.Equal(t, http.MethodPost, method)
}

func TestUpdatePlanStatusTool(t *testing.T) {
	const updatedJSON = `{"id":"plan-ab12cd34","project_id":"/tmp/proj","name":"feature-x",
		"file_path":"plans/in_progress/feature-x.yaml","status":"in_progress",
		"created_at":"2026-09-28T00:00:00Z","updated_at":"2026-09-28T02:00:00Z"}`
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/projects/") && strings.Contains(r.URL.Path, "/plans/plan-ab12cd34") && r.Method == http.MethodPatch {
			_, _ = w.Write([]byte(updatedJSON))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer daemon.Close()
	session := connectTo(t, daemon.URL)

	res, err := session.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name:      "update_plan_status",
		Arguments: map[string]any{"project_id": "/tmp/proj", "plan_id": "plan-ab12cd34", "status": "in_progress"},
	})
	require.NoError(t, err)
	require.False(t, res.IsError, textOf(res))
	require.Contains(t, textOf(res), `"in_progress"`)
}

func TestArchivePlanTool(t *testing.T) {
	const archivedJSON = `{"id":"plan-ab12cd34","project_id":"/tmp/proj","name":"feature-x",
		"file_path":"plans/archived/feature-x.yaml","status":"archived",
		"created_at":"2026-09-28T00:00:00Z","updated_at":"2026-09-28T03:00:00Z"}`
	var path string
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/archive") && r.Method == http.MethodPost {
			path = r.URL.Path
			_, _ = w.Write([]byte(archivedJSON))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer daemon.Close()
	session := connectTo(t, daemon.URL)

	res, err := session.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name:      "archive_plan",
		Arguments: map[string]any{"plan_id": "plan-ab12cd34"},
	})
	require.NoError(t, err)
	require.False(t, res.IsError, textOf(res))
	require.Contains(t, textOf(res), `"archived"`)
	require.Equal(t, "/api/v1/plans/plan-ab12cd34/archive", path)
}

func TestRunPlanTool(t *testing.T) {
	const runningJSON = `{"id":"plan-ab12cd34","project_id":"/tmp/proj","name":"feature-x",
		"file_path":"plans/in_progress/feature-x.yaml","status":"in_progress",
		"execution_mode":"manual",
		"created_at":"2026-09-28T00:00:00Z","updated_at":"2026-09-28T02:00:00Z"}`
	var body string
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/run") && r.Method == http.MethodPost {
			b, _ := io.ReadAll(r.Body)
			body = string(b)
			_, _ = w.Write([]byte(runningJSON))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer daemon.Close()
	session := connectTo(t, daemon.URL)

	res, err := session.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name:      "run_plan",
		Arguments: map[string]any{"plan_id": "plan-ab12cd34", "execution_mode": "manual"},
	})
	require.NoError(t, err)
	require.False(t, res.IsError, textOf(res))
	require.Contains(t, textOf(res), `"in_progress"`)
	require.Contains(t, textOf(res), `"manual"`)
	require.Contains(t, body, `"execution_mode":"manual"`)
}

func TestUpdateTaskStatusPlanForm(t *testing.T) {
	var path, body string
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/tasks/t1/status") && r.Method == http.MethodPost {
			b, _ := io.ReadAll(r.Body)
			path, body = r.URL.Path, string(b)
			_, _ = w.Write([]byte(samplePlanJSON))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer daemon.Close()
	session := connectTo(t, daemon.URL)

	res, err := session.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name:      "update_task_status",
		Arguments: map[string]any{"plan_id": "plan-ab12cd34", "task_id": "t1", "status": "done"},
	})
	require.NoError(t, err)
	require.False(t, res.IsError, textOf(res))
	require.Equal(t, "/api/v1/plans/plan-ab12cd34/tasks/t1/status", path)
	require.Contains(t, body, `"status":"done"`)
	require.Contains(t, textOf(res), `"plan-ab12cd34"`)
}

func TestUpdateTaskStatusAutopilotForm(t *testing.T) {
	var path, body string
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/autopilot/tasks/status" {
			b, _ := io.ReadAll(r.Body)
			path, body = r.URL.Path, string(b)
			_, _ = w.Write([]byte(`{"id":"t1","status":"done","landed_pr":12}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer daemon.Close()
	session := connectTo(t, daemon.URL)

	res, err := session.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name:      "update_task_status",
		Arguments: map[string]any{"run_id": "run-1", "task_id": "t1", "status": "done", "landed_pr": 12},
	})
	require.NoError(t, err)
	require.False(t, res.IsError, textOf(res))
	require.Equal(t, "/api/v1/autopilot/tasks/status", path)
	require.Contains(t, body, `"run_id":"run-1"`)
	require.Contains(t, textOf(res), `"done"`)
}

func TestCompletePlanTool(t *testing.T) {
	const doneJSON = `{"id":"plan-ab12cd34","project_id":"/tmp/proj","name":"feature-x",
		"file_path":"plans/completed/feature-x.yaml","status":"completed",
		"created_at":"2026-09-28T00:00:00Z","updated_at":"2026-09-28T04:00:00Z"}`
	var path string
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/complete") && r.Method == http.MethodPost {
			path = r.URL.Path
			_, _ = w.Write([]byte(doneJSON))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer daemon.Close()
	session := connectTo(t, daemon.URL)

	res, err := session.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name:      "complete_plan",
		Arguments: map[string]any{"plan_id": "plan-ab12cd34"},
	})
	require.NoError(t, err)
	require.False(t, res.IsError, textOf(res))
	require.Contains(t, textOf(res), `"completed"`)
	require.Equal(t, "/api/v1/plans/plan-ab12cd34/complete", path)
}

func TestCompletePlanToolStructuredError(t *testing.T) {
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/complete") {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error":             "incomplete tasks: t1; unmerged branches: feat/x",
				"incomplete_tasks":  []string{"t1"},
				"unmerged_branches": []string{"feat/x"},
			})
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer daemon.Close()
	session := connectTo(t, daemon.URL)

	res, err := session.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name:      "complete_plan",
		Arguments: map[string]any{"plan_id": "plan-ab12cd34"},
	})
	require.NoError(t, err)
	out := textOf(res)
	require.Contains(t, out, `"incomplete_tasks"`)
	require.Contains(t, out, `"t1"`)
	require.Contains(t, out, `"unmerged_branches"`)
	require.Contains(t, out, `"feat/x"`)
	require.Contains(t, out, "incomplete tasks")
}
