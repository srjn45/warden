package client

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUsageRefreshAndSchemaValidation(t *testing.T) {
	var query string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"schema_version":1,"generated_at":"2026-09-01T10:00:00Z","backends":[{"id":"antigravity","tier":"subscription","installed":true,"enabled":true,"status":"ok","account":{"plan":"pro"},"usage":[{"id":"antigravity:gemini-5h","scope":"gemini","label":"Gemini 5-hour","model_families":["gemini"],"models":null,"used_percent":50,"resets_at":"2026-09-01T12:00:00Z"},{"id":"antigravity:non-gemini-weekly","scope":"non-gemini","label":"Non-Gemini weekly","model_families":null,"models":null,"used_percent":null,"resets_at":null}],"observed_at":"2026-09-01T10:00:00Z","cached":false,"stale":false}]}`))
	}))
	defer ts.Close()
	got, err := New(ts.URL).Usage(t.Context(), true)
	require.NoError(t, err)
	require.Equal(t, "refresh=true", query)
	require.Equal(t, 1, got.SchemaVersion)
	require.Len(t, got.Backends[0].Usage, 2)
	require.Equal(t, "gemini", got.Backends[0].Usage[0].Scope)
	require.Nil(t, got.Backends[0].Usage[1].UsedPercent)
	require.Nil(t, got.Backends[0].Usage[1].ResetsAt)
}

func TestUsageRejectsUnknownSchema(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"schema_version":2,"generated_at":"2026-09-01T10:00:00Z","backends":[]}`))
	}))
	defer ts.Close()
	_, err := New(ts.URL).Usage(t.Context(), false)
	require.ErrorContains(t, err, "schema version")
}

func TestUsageRecoverClientPostsFilters(t *testing.T) {
	var method, path, body string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"dry_run":true,
			"provider_usage":{"schema_version":1,"generated_at":"2026-10-01T00:00:00Z","backends":[]},
			"snapshots":[],
			"impact":{"exhausted_buckets":[],"affected_agents":[],"skipped_agents":[],"stale_or_unknown":[]},
			"outcomes":[{"agent_id":"a1","outcome":"would_start","bucket_key":"weekly","selected":{"backend_id":"claude","model_id":"sonnet"},"candidates":[{"backend_id":"claude","model_id":"sonnet"}]}],
			"max_parallel_swaps":2
		}`))
	}))
	defer ts.Close()

	got, err := New(ts.URL).UsageRecover(t.Context(), UsageRecoverParams{
		DryRun: true, AiCli: "codex", Project: "/tmp/proj", MaxParallelSwaps: 2,
	})
	require.NoError(t, err)
	require.Equal(t, http.MethodPost, method)
	require.Equal(t, "/api/v1/usage/recover", path)
	require.Contains(t, body, `"dry_run":true`)
	require.Contains(t, body, `"ai_cli":"codex"`)
	require.Contains(t, body, `"project":"/tmp/proj"`)
	require.Contains(t, body, `"max_parallel_swaps":2`)
	require.True(t, got.DryRun)
	require.Equal(t, 2, got.MaxParallelSwaps)
	require.Len(t, got.Outcomes, 1)
	require.Equal(t, "would_start", got.Outcomes[0].Outcome)
	require.Equal(t, "claude", got.Outcomes[0].Selected.BackendID)
}
