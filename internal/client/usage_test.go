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
		_, _ = w.Write([]byte(`{"schema_version":1,"generated_at":"2026-09-01T10:00:00Z","backends":[]}`))
	}))
	defer ts.Close()
	got, err := New(ts.URL).Usage(t.Context(), true)
	require.NoError(t, err)
	require.Equal(t, "refresh=true", query)
	require.Equal(t, 1, got.SchemaVersion)
}

func TestUsageRejectsUnknownSchema(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"schema_version":2,"generated_at":"2026-09-01T10:00:00Z","backends":[]}`))
	}))
	defer ts.Close()
	_, err := New(ts.URL).Usage(t.Context(), false)
	require.ErrorContains(t, err, "schema version")
}
