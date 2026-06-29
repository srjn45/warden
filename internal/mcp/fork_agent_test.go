package mcp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

// TestForkAgentToolPlumbsForkFrom proves the MCP fork_agent twin wraps the spawn API
// with fork_from set (CLI/MCP parity with `warden fork`): it sends fork_from + the
// divergent prompt and defaults the type to a worktree-backed one.
func TestForkAgentToolPlumbsForkFrom(t *testing.T) {
	t.Setenv("WARDEN_SESSION_ID", "")
	var gotBody string
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/spawn" {
			b, _ := io.ReadAll(r.Body)
			gotBody = string(b)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"development-9z","type":"development","status":"spawning"}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer daemon.Close()

	srv := NewServer(daemon.URL)
	ctx := context.Background()
	clientTransport, serverTransport := mcpsdk.NewInMemoryTransports()
	go func() { _ = srv.Run(ctx, serverTransport) }()

	cl := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "0"}, nil)
	session, err := cl.Connect(ctx, clientTransport, nil)
	require.NoError(t, err)
	defer session.Close()

	res, err := session.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "fork_agent",
		Arguments: map[string]any{"source": "src-agent", "prompt": "try plan B"},
	})
	require.NoError(t, err)
	require.False(t, res.IsError, textOf(res))
	require.Contains(t, gotBody, `"fork_from":"src-agent"`)
	require.Contains(t, gotBody, `"type":"development"`)
	require.Contains(t, gotBody, `"prompt":"try plan B"`)
}

// TestForkAgentToolRequiresSource asserts the missing-source guard fires before any
// daemon call.
func TestForkAgentToolRequiresSource(t *testing.T) {
	called := false
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		_, _ = w.Write([]byte(`{}`))
	}))
	defer daemon.Close()

	srv := NewServer(daemon.URL)
	ctx := context.Background()
	clientTransport, serverTransport := mcpsdk.NewInMemoryTransports()
	go func() { _ = srv.Run(ctx, serverTransport) }()

	cl := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "0"}, nil)
	session, err := cl.Connect(ctx, clientTransport, nil)
	require.NoError(t, err)
	defer session.Close()

	res, err := session.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "fork_agent",
		Arguments: map[string]any{"source": "   ", "prompt": "blank source"},
	})
	require.NoError(t, err)
	require.Contains(t, textOf(res), "source agent id is required")
	require.False(t, called, "the guard must fire before contacting the daemon")
}
