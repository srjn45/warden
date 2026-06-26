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

func TestListAgentsTool(t *testing.T) {
	// Fake daemon returning one session.
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"sessions":[{"id":"A-1","status":"working"}]}`))
	}))
	defer daemon.Close()

	srv := NewServer(daemon.URL)

	// Connect an in-memory client to the server over an in-process transport.
	ctx := context.Background()
	clientTransport, serverTransport := mcpsdk.NewInMemoryTransports()
	go func() { _ = srv.Run(ctx, serverTransport) }()

	cl := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "0"}, nil)
	session, err := cl.Connect(ctx, clientTransport, nil)
	require.NoError(t, err)
	defer session.Close()

	res, err := session.CallTool(ctx, &mcpsdk.CallToolParams{Name: "list_agents"})
	require.NoError(t, err)
	require.False(t, res.IsError)
	require.Contains(t, textOf(res), "A-1")
}

func TestSpawnAgentToolSendsPrompt(t *testing.T) {
	var gotBody string
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/spawn" {
			b, _ := io.ReadAll(r.Body)
			gotBody = string(b)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"agent-x","status":"spawning"}`))
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
		Name:      "spawn_agent",
		Arguments: map[string]any{"prompt": "research SSE reconnection"},
	})
	require.NoError(t, err)
	require.False(t, res.IsError, textOf(res))
	require.Contains(t, gotBody, `"prompt":"research SSE reconnection"`)
}

func textOf(res *mcpsdk.CallToolResult) string {
	out := ""
	for _, c := range res.Content {
		if tc, ok := c.(*mcpsdk.TextContent); ok {
			out += tc.Text
		}
	}
	return out
}

func TestSpawnAgentToolSendsPermissionMode(t *testing.T) {
	var gotBody string
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/spawn" {
			b, _ := io.ReadAll(r.Body)
			gotBody = string(b)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"agent-x","status":"spawning"}`))
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
		Name:      "spawn_agent",
		Arguments: map[string]any{"prompt": "do X", "permission_mode": "acceptEdits"},
	})
	require.NoError(t, err)
	require.False(t, res.IsError, textOf(res))
	require.Contains(t, gotBody, `"permission_mode":"acceptEdits"`)
}

func TestSpawnAgentToolSendsExplicitDirAsCwd(t *testing.T) {
	var gotBody string
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/spawn" {
			b, _ := io.ReadAll(r.Body)
			gotBody = string(b)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"agent-x","status":"spawning"}`))
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
		Name:      "spawn_agent",
		Arguments: map[string]any{"prompt": "do X", "dir": "/work/proj"},
	})
	require.NoError(t, err)
	require.False(t, res.IsError, textOf(res))
	require.Contains(t, gotBody, `"cwd":"/work/proj"`)
}

func TestRestoreAgentTool(t *testing.T) {
	var hitPath string
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/sessions/A-1/restore" {
			hitPath = r.URL.Path
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"restoring"}`))
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
		Name:      "restore_agent",
		Arguments: map[string]any{"ticket": "A-1"},
	})
	require.NoError(t, err)
	require.False(t, res.IsError, textOf(res))
	require.Equal(t, "/api/v1/sessions/A-1/restore", hitPath)
}

func TestAdoptAgentTool(t *testing.T) {
	var hitPath string
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/adopt" {
			hitPath = r.URL.Path
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"session":{"id":"adopted-1","status":"working"},"warning":""}`))
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
		Name:      "adopt_agent",
		Arguments: map[string]any{"dir": "/work/proj"},
	})
	require.NoError(t, err)
	require.False(t, res.IsError, textOf(res))
	require.Equal(t, "/api/v1/adopt", hitPath)
	require.Contains(t, textOf(res), "adopted-1")
}

func TestTeardownTools(t *testing.T) {
	hits := map[string]bool{}
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits[r.URL.Path] = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer daemon.Close()
	srv := NewServer(daemon.URL)
	ctx := context.Background()
	ct, st := mcpsdk.NewInMemoryTransports()
	go func() { _ = srv.Run(ctx, st) }()
	cl := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "0"}, nil)
	session, err := cl.Connect(ctx, ct, nil)
	require.NoError(t, err)
	defer session.Close()

	for _, tc := range []struct{ tool, path string }{
		{"terminate_agent", "/api/v1/sessions/A-1/terminate"},
		{"delete_agent", "/api/v1/sessions/A-1/delete"},
		{"remove_worktree", "/api/v1/sessions/A-1/remove-worktree"},
	} {
		res, err := session.CallTool(ctx, &mcpsdk.CallToolParams{Name: tc.tool, Arguments: map[string]any{"ticket": "A-1"}})
		require.NoError(t, err)
		require.False(t, res.IsError, textOf(res))
		require.True(t, hits[tc.path], "expected %s to hit %s", tc.tool, tc.path)
	}
}

// TestStopAgentTool: the umbrella stop_agent default is a full teardown
// (terminate + delete record + remove worktree); keep_* flags are subtractive.
func TestStopAgentTool(t *testing.T) {
	newSession := func(t *testing.T) (*mcpsdk.ClientSession, map[string]bool) {
		hits := map[string]bool{}
		daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits[r.URL.Path] = true
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		}))
		t.Cleanup(daemon.Close)
		srv := NewServer(daemon.URL)
		ctx := context.Background()
		ct, st := mcpsdk.NewInMemoryTransports()
		go func() { _ = srv.Run(ctx, st) }()
		cl := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "0"}, nil)
		session, err := cl.Connect(ctx, ct, nil)
		require.NoError(t, err)
		t.Cleanup(func() { session.Close() })
		return session, hits
	}

	t.Run("default full teardown", func(t *testing.T) {
		session, hits := newSession(t)
		res, err := session.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "stop_agent", Arguments: map[string]any{"ticket": "A-1"}})
		require.NoError(t, err)
		require.False(t, res.IsError, textOf(res))
		require.True(t, hits["/api/v1/sessions/A-1/terminate"])
		require.True(t, hits["/api/v1/sessions/A-1/delete"])
		require.True(t, hits["/api/v1/sessions/A-1/remove-worktree"])
	})

	t.Run("keep_worktree skips worktree removal", func(t *testing.T) {
		session, hits := newSession(t)
		res, err := session.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "stop_agent", Arguments: map[string]any{"ticket": "A-1", "keep_worktree": true}})
		require.NoError(t, err)
		require.False(t, res.IsError, textOf(res))
		require.True(t, hits["/api/v1/sessions/A-1/terminate"])
		require.True(t, hits["/api/v1/sessions/A-1/delete"])
		require.False(t, hits["/api/v1/sessions/A-1/remove-worktree"])
	})

	t.Run("keep_record skips delete", func(t *testing.T) {
		session, hits := newSession(t)
		res, err := session.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "stop_agent", Arguments: map[string]any{"ticket": "A-1", "keep_record": true}})
		require.NoError(t, err)
		require.False(t, res.IsError, textOf(res))
		require.True(t, hits["/api/v1/sessions/A-1/terminate"])
		require.False(t, hits["/api/v1/sessions/A-1/delete"])
		require.True(t, hits["/api/v1/sessions/A-1/remove-worktree"])
	})
}

func TestListApprovalsTool(t *testing.T) {
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/approvals" {
			_, _ = w.Write([]byte(`{"enabled":true,"approvals":[{"id":"agent-1","action":"Bash","question":"Run rm?","options":["Yes","No"],"fingerprint":"abc","recognized":true}]}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer daemon.Close()

	srv := NewServer(daemon.URL)
	ctx := context.Background()
	ct, st := mcpsdk.NewInMemoryTransports()
	go func() { _ = srv.Run(ctx, st) }()
	cl := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "0"}, nil)
	session, err := cl.Connect(ctx, ct, nil)
	require.NoError(t, err)
	defer session.Close()

	res, err := session.CallTool(ctx, &mcpsdk.CallToolParams{Name: "list_approvals"})
	require.NoError(t, err)
	require.False(t, res.IsError, textOf(res))
	require.Contains(t, textOf(res), "agent-1")
}

func TestListApprovalsToolDisabled(t *testing.T) {
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"enabled":false,"approvals":[]}`))
	}))
	defer daemon.Close()

	srv := NewServer(daemon.URL)
	ctx := context.Background()
	ct, st := mcpsdk.NewInMemoryTransports()
	go func() { _ = srv.Run(ctx, st) }()
	cl := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "0"}, nil)
	session, err := cl.Connect(ctx, ct, nil)
	require.NoError(t, err)
	defer session.Close()

	res, err := session.CallTool(ctx, &mcpsdk.CallToolParams{Name: "list_approvals"})
	require.NoError(t, err)
	require.False(t, res.IsError, textOf(res))
	require.Contains(t, textOf(res), "approvals disabled")
}

func TestApproveTool(t *testing.T) {
	var gotPath, gotBody string
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/approvals" {
			_, _ = w.Write([]byte(`{"enabled":true,"approvals":[{"id":"agent-1","action":"Bash","question":"Run rm?","options":["Yes","No"],"fingerprint":"abc","recognized":true}]}`))
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/sessions/agent-1/approve" {
			gotPath = r.URL.Path
			b, _ := io.ReadAll(r.Body)
			gotBody = string(b)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer daemon.Close()

	srv := NewServer(daemon.URL)
	ctx := context.Background()
	ct, st := mcpsdk.NewInMemoryTransports()
	go func() { _ = srv.Run(ctx, st) }()
	cl := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "0"}, nil)
	session, err := cl.Connect(ctx, ct, nil)
	require.NoError(t, err)
	defer session.Close()

	res, err := session.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "approve",
		Arguments: map[string]any{"ticket": "agent-1", "option": 1},
	})
	require.NoError(t, err)
	require.False(t, res.IsError, textOf(res))
	require.Equal(t, "/api/v1/sessions/agent-1/approve", gotPath)
	require.Contains(t, gotBody, `"option":1`)
	require.Contains(t, gotBody, `"fingerprint":"abc"`)
}

func TestApproveToolDisabled(t *testing.T) {
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"enabled":false,"approvals":[]}`))
	}))
	defer daemon.Close()

	srv := NewServer(daemon.URL)
	ctx := context.Background()
	ct, st := mcpsdk.NewInMemoryTransports()
	go func() { _ = srv.Run(ctx, st) }()
	cl := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "0"}, nil)
	session, err := cl.Connect(ctx, ct, nil)
	require.NoError(t, err)
	defer session.Close()

	res, err := session.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "approve",
		Arguments: map[string]any{"ticket": "agent-1", "option": 1},
	})
	require.NoError(t, err)
	require.False(t, res.IsError, textOf(res))
	require.Contains(t, textOf(res), "approvals disabled")
}

func TestApproveToolUnrecognized(t *testing.T) {
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/approvals" {
			_, _ = w.Write([]byte(`{"enabled":true,"approvals":[{"id":"agent-1","recognized":false}]}`))
			return
		}
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer daemon.Close()

	srv := NewServer(daemon.URL)
	ctx := context.Background()
	ct, st := mcpsdk.NewInMemoryTransports()
	go func() { _ = srv.Run(ctx, st) }()
	cl := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "0"}, nil)
	session, err := cl.Connect(ctx, ct, nil)
	require.NoError(t, err)
	defer session.Close()

	res, err := session.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "approve",
		Arguments: map[string]any{"ticket": "agent-1", "option": 1},
	})
	require.NoError(t, err)
	require.False(t, res.IsError, textOf(res))
	require.Contains(t, textOf(res), "not a recognized menu")
}

func TestCtxSetClientPath(t *testing.T) {
	var gotPath, gotMethod string
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		w.Write([]byte(`{"key":"global.k","value":"v","updated_by":"agent"}`))
	}))
	defer daemon.Close()

	srv := NewServer(daemon.URL)
	if _, err := srv.cl.CtxSet(context.Background(), "global.k", "v", "agent"); err != nil {
		t.Fatalf("CtxSet: %v", err)
	}
	if gotMethod != http.MethodPut || gotPath != "/api/v1/context/global.k" {
		t.Fatalf("got %s %s", gotMethod, gotPath)
	}
}

func TestSendMessageClientPath(t *testing.T) {
	var gotPath, gotMethod string
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		w.Write([]byte(`{"message":{"id":"1","from":"agent","to":"agent-1","body":"hi"},"woke":false}`))
	}))
	defer daemon.Close()

	srv := NewServer(daemon.URL)
	if _, _, err := srv.cl.MsgSend(context.Background(), "agent-1", "agent", "hi"); err != nil {
		t.Fatalf("MsgSend: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/v1/sessions/agent-1/messages" {
		t.Fatalf("got %s %s", gotMethod, gotPath)
	}
}

func TestPipelineTools(t *testing.T) {
	hits := map[string]string{} // path -> method
	bodies := map[string]string{}
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		key := r.Method + " " + r.URL.Path
		hits[r.URL.Path] = r.Method
		bodies[key] = string(b)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/pipelines":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"demo","name":"demo","status":"pending","jobs":[{"id":"a"}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/pipelines":
			_, _ = w.Write([]byte(`{"pipelines":[{"id":"demo","status":"pending","jobs":[]}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/pipelines/demo":
			_, _ = w.Write([]byte(`{"id":"demo","status":"running","jobs":[{"id":"a","status":"done"}]}`))
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		}
	}))
	defer daemon.Close()

	srv := NewServer(daemon.URL)
	ctx := context.Background()
	ct, st := mcpsdk.NewInMemoryTransports()
	go func() { _ = srv.Run(ctx, st) }()
	cl := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "0"}, nil)
	session, err := cl.Connect(ctx, ct, nil)
	require.NoError(t, err)
	defer session.Close()

	// create_pipeline forwards the raw spec and returns the created pipeline.
	res, err := session.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "create_pipeline",
		Arguments: map[string]any{"spec": "name: demo\nrepo: /r\njobs: []\n"},
	})
	require.NoError(t, err)
	require.False(t, res.IsError, textOf(res))
	require.Contains(t, bodies["POST /api/v1/pipelines"], "name: demo")
	require.Contains(t, textOf(res), "demo")

	res, err = session.CallTool(ctx, &mcpsdk.CallToolParams{Name: "list_pipelines"})
	require.NoError(t, err)
	require.False(t, res.IsError, textOf(res))
	require.Contains(t, textOf(res), "demo")

	res, err = session.CallTool(ctx, &mcpsdk.CallToolParams{
		Name: "show_pipeline", Arguments: map[string]any{"pipeline": "demo"}})
	require.NoError(t, err)
	require.False(t, res.IsError, textOf(res))
	require.Contains(t, textOf(res), "running")

	res, err = session.CallTool(ctx, &mcpsdk.CallToolParams{
		Name: "start_pipeline", Arguments: map[string]any{"pipeline": "demo"}})
	require.NoError(t, err)
	require.False(t, res.IsError, textOf(res))
	require.Equal(t, http.MethodPost, hits["/api/v1/pipelines/demo/start"])

	res, err = session.CallTool(ctx, &mcpsdk.CallToolParams{
		Name: "cancel_pipeline", Arguments: map[string]any{"pipeline": "demo"}})
	require.NoError(t, err)
	require.False(t, res.IsError, textOf(res))
	require.Equal(t, http.MethodPost, hits["/api/v1/pipelines/demo/cancel"])
}
