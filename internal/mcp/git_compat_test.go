package mcp

// COMPATIBILITY SUITE — MCP git/check tools (task t1-compat-baseline).
//
// The `commit`, `push`, `sync` and `check` tools and their parameter names are
// part of the contract installed agents and the PreToolUse redirects rely on
// (mcp__warden__commit etc.). Pinned: tool presence, exact parameter-name sets,
// absence of required parameters, and the request body each tool sends.
//
// Later tasks may only change an assertion in this file when their task prompt
// says so, and must say why in the commit message.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func TestCompatGitCheckToolsAndParams(t *testing.T) {
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) }))
	defer daemon.Close()
	session := connectTo(t, daemon.URL)

	want := map[string][]string{
		"commit": {"dir", "message"},
		"push":   {"dir", "force"},
		"sync":   {"base", "dir"},
		"check":  {"dir", "name"},
	}
	got := map[string]*mcpsdk.Tool{}
	for tool, err := range session.Tools(context.Background(), nil) {
		require.NoError(t, err)
		got[tool.Name] = tool
	}
	for name, params := range want {
		tool, ok := got[name]
		require.Truef(t, ok, "tool %q must be registered", name)
		raw, err := json.Marshal(tool.InputSchema)
		require.NoError(t, err)
		var schema struct {
			Properties map[string]struct {
				Type string `json:"type"`
			} `json:"properties"`
			Required []string `json:"required"`
		}
		require.NoError(t, json.Unmarshal(raw, &schema))
		var have []string
		for p := range schema.Properties {
			have = append(have, p)
		}
		sort.Strings(have)
		require.Equal(t, params, have, "parameter names of %q", name)
		require.Empty(t, schema.Required, "%q has no required parameters", name)
		require.NotEmpty(t, tool.Description)
	}
	require.Equal(t, "boolean", mustType(t, got["push"], "force"))
	require.Equal(t, "string", mustType(t, got["commit"], "message"))
}

func mustType(t *testing.T, tool *mcpsdk.Tool, prop string) string {
	t.Helper()
	raw, _ := json.Marshal(tool.InputSchema)
	var s struct {
		Properties map[string]struct {
			Type string `json:"type"`
		} `json:"properties"`
	}
	require.NoError(t, json.Unmarshal(raw, &s))
	return s.Properties[prop].Type
}

// Each tool POSTs its parameters under the same JSON keys to the daemon route.
func TestCompatGitCheckToolRequestBodies(t *testing.T) {
	t.Setenv("WARDEN_SESSION_ID", "compat-agent")
	for _, tc := range []struct {
		tool, path string
		args       map[string]any
		wantKeys   map[string]any
	}{
		{"commit", "/api/v1/git/commit", map[string]any{"message": "m"}, map[string]any{"message": "m", "session": "compat-agent"}},
		{"push", "/api/v1/git/push", map[string]any{"force": true}, map[string]any{"force": true, "session": "compat-agent"}},
		{"sync", "/api/v1/git/sync", map[string]any{"base": "integ"}, map[string]any{"base": "integ", "session": "compat-agent"}},
		{"check", "/api/v1/check", map[string]any{"name": "test"}, map[string]any{"name": "test", "session": "compat-agent"}},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			bodies := make(chan map[string]any, 1)
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, tc.path, r.URL.Path)
				var b map[string]any
				_ = json.NewDecoder(r.Body).Decode(&b)
				bodies <- b
				_, _ = w.Write([]byte(`{}`))
			}))
			defer daemon.Close()
			res, err := connectTo(t, daemon.URL).CallTool(context.Background(), &mcpsdk.CallToolParams{Name: tc.tool, Arguments: tc.args})
			require.NoError(t, err)
			require.False(t, res.IsError, textOf(res))
			body := <-bodies
			for k, v := range tc.wantKeys {
				require.Equal(t, v, body[k], "%s body key %q", tc.tool, k)
			}
		})
	}
}
