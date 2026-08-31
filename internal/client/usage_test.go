package client

import (
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
		_, _ = w.Write([]byte(`{"schema_version":1,"generated_at":"2026-09-01T10:00:00Z","backends":[{"id":"antigravity","tier":"subscription","installed":true,"enabled":true,"status":"ok","account":{"plan":"pro"},"usage":[{"id":"antigravity:gemini","scope":"gemini","label":"Gemini models","model_families":["gemini"],"models":null,"used_percent":50,"resets_at":"2026-09-01T12:00:00Z"},{"id":"antigravity:non-gemini","scope":"non-gemini","label":"Non-Gemini models","model_families":null,"models":null,"used_percent":null,"resets_at":null}],"observed_at":"2026-09-01T10:00:00Z","cached":false,"stale":false}]}`))
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
