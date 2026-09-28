package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

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
		"list_plans", "get_plan", "create_plan", "scan_plans",
		"update_plan_status", "archive_plan", "assess_plan", "run_plan",
	}
	for _, name := range want {
		require.Truef(t, got[name], "tool %q should be registered", name)
	}
}

// TestListPlansTool exercises the list_plans tool round-trip via a stub daemon.
func TestListPlansTool(t *testing.T) {
	const plansJSON = `{"plans":[
		{"id":"plan-ab12cd34","project_id":"/tmp/proj","name":"feature-x",
		 "file_path":"plans/pending/feature-x.yaml","status":"pending",
		 "created_at":"2026-09-28T00:00:00Z","updated_at":"2026-09-28T00:00:00Z"}
	]}`
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/plans") && r.Method == http.MethodGet {
			_, _ = w.Write([]byte(plansJSON))
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

// TestGetPlanTool exercises the get_plan tool.
func TestGetPlanTool(t *testing.T) {
	const planJSON = `{"id":"plan-ab12cd34","project_id":"/tmp/proj","name":"feature-x",
		"file_path":"plans/pending/feature-x.yaml","status":"pending",
		"created_at":"2026-09-28T00:00:00Z","updated_at":"2026-09-28T00:00:00Z"}`
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/plans/plan-ab12cd34") && r.Method == http.MethodGet {
			_, _ = w.Write([]byte(planJSON))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer daemon.Close()
	session := connectTo(t, daemon.URL)

	res, err := session.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name:      "get_plan",
		Arguments: map[string]any{"project_id": "/tmp/proj", "plan_id": "plan-ab12cd34"},
	})
	require.NoError(t, err)
	require.False(t, res.IsError, textOf(res))
	require.Contains(t, textOf(res), `"plan-ab12cd34"`)
	require.Contains(t, textOf(res), `"pending"`)
}

// TestScanPlansTool exercises the scan_plans tool.
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

// TestUpdatePlanStatusTool exercises the update_plan_status tool.
func TestUpdatePlanStatusTool(t *testing.T) {
	const updatedJSON = `{"id":"plan-ab12cd34","project_id":"/tmp/proj","name":"feature-x",
		"file_path":"plans/in_progress/feature-x.yaml","status":"in_progress",
		"created_at":"2026-09-28T00:00:00Z","updated_at":"2026-09-28T02:00:00Z"}`
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/plans/plan-ab12cd34") && r.Method == http.MethodPatch {
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

// TestArchivePlanTool exercises the archive_plan tool.
func TestArchivePlanTool(t *testing.T) {
	const archivedJSON = `{"id":"plan-ab12cd34","project_id":"/tmp/proj","name":"feature-x",
		"file_path":"plans/archived/feature-x.yaml","status":"archived",
		"created_at":"2026-09-28T00:00:00Z","updated_at":"2026-09-28T03:00:00Z"}`
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/plans/plan-ab12cd34") && r.Method == http.MethodPatch {
			_, _ = w.Write([]byte(archivedJSON))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer daemon.Close()
	session := connectTo(t, daemon.URL)

	res, err := session.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name:      "archive_plan",
		Arguments: map[string]any{"project_id": "/tmp/proj", "plan_id": "plan-ab12cd34"},
	})
	require.NoError(t, err)
	require.False(t, res.IsError, textOf(res))
	require.Contains(t, textOf(res), `"archived"`)
}
