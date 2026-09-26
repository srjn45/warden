package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGitToolsForwardExplicitDirAndSession(t *testing.T) {
	t.Setenv("WARDEN_SESSION_ID", "forward-agent")
	for _, tc := range []struct{ tool, path string }{
		{"commit", "/api/v1/git/commit"},
		{"push", "/api/v1/git/push"},
		{"sync", "/api/v1/git/sync"},
		{"check", "/api/v1/check"},
		{"snapshot_create", "/api/v1/snapshots"},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			requests := make(chan map[string]any, 1)
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodPost, r.Method)
				assert.Equal(t, tc.path, r.URL.Path)
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				requests <- body
				_, _ = w.Write([]byte(`{}`))
			}))
			defer daemon.Close()
			session := connectTo(t, daemon.URL)
			dir := filepath.Join("relative", "linked-worktree")
			expected, err := filepath.Abs(dir)
			require.NoError(t, err)
			result, err := session.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: tc.tool, Arguments: map[string]any{"dir": dir}})
			require.NoError(t, err)
			require.False(t, result.IsError, textOf(result))
			body := <-requests
			require.Equal(t, expected, body["dir"])
			require.Equal(t, "forward-agent", body["session"])
		})
	}
}
