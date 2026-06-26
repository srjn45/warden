package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/srjn45/warden/internal/approval"
	"github.com/stretchr/testify/require"
)

// connect wires an in-memory MCP client to a Server bound to the fake daemon at
// daemonURL, mirroring the harness already used across server_test.go.
func connect(t *testing.T, daemonURL string) *mcpsdk.ClientSession {
	t.Helper()
	srv := NewServer(daemonURL)
	ctx := context.Background()
	ct, st := mcpsdk.NewInMemoryTransports()
	go func() { _ = srv.Run(ctx, st) }()
	cl := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "0"}, nil)
	session, err := cl.Connect(ctx, ct, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// fakeDaemon serves canned JSON keyed by "METHOD PATH"; an unmatched route gets
// `{}` so a tool that makes an incidental call doesn't error. It records every
// request path+body it saw.
type recordedReq struct {
	method string
	body   string
}

func fakeDaemon(t *testing.T, routes map[string]string, seen map[string]recordedReq) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		if r.ContentLength > 0 {
			_, _ = r.Body.Read(buf)
		}
		if seen != nil {
			seen[r.URL.Path] = recordedReq{method: r.Method, body: string(buf)}
		}
		w.Header().Set("Content-Type", "application/json")
		if body, ok := routes[r.Method+" "+r.URL.Path]; ok {
			_, _ = w.Write([]byte(body))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(ts.Close)
	return ts
}

func call(t *testing.T, s *mcpsdk.ClientSession, name string, args map[string]any) *mcpsdk.CallToolResult {
	t.Helper()
	res, err := s.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: name, Arguments: args})
	require.NoError(t, err)
	return res
}

func TestGetAgentTool(t *testing.T) {
	seen := map[string]recordedReq{}
	ts := fakeDaemon(t, map[string]string{
		"GET /sessions/A-1": `{"id":"A-1","status":"working"}`,
	}, seen)
	s := connect(t, ts.URL)
	res := call(t, s, "get_agent", map[string]any{"ticket": "A-1"})
	require.False(t, res.IsError, textOf(res))
	require.Contains(t, textOf(res), "A-1")
	require.Equal(t, http.MethodGet, seen["/sessions/A-1"].method)
}

func TestSendToAgentTool(t *testing.T) {
	seen := map[string]recordedReq{}
	ts := fakeDaemon(t, nil, seen)
	s := connect(t, ts.URL)
	res := call(t, s, "send_to_agent", map[string]any{"ticket": "A-1", "text": "hello"})
	require.False(t, res.IsError, textOf(res))
	require.Contains(t, textOf(res), "sent to A-1")
	require.Equal(t, http.MethodPost, seen["/sessions/A-1/input"].method)
	require.Contains(t, seen["/sessions/A-1/input"].body, `"text":"hello"`)
}

func TestGetAgentOutputTool(t *testing.T) {
	ts := fakeDaemon(t, map[string]string{
		"GET /sessions/A-1/output": `{"output":"recent pane text"}`,
	}, nil)
	s := connect(t, ts.URL)
	res := call(t, s, "get_agent_output", map[string]any{"ticket": "A-1", "lines": 10})
	require.False(t, res.IsError, textOf(res))
	require.Contains(t, textOf(res), "recent pane text")
}

func TestGitToolsForwardResults(t *testing.T) {
	seen := map[string]recordedReq{}
	ts := fakeDaemon(t, map[string]string{
		"POST /git/commit": `{"committed":true,"sha":"abc","branch":"feat","files":["a.go"]}`,
		"POST /git/push":   `{"branch":"feat","remote":"origin","pushed":true}`,
		"POST /git/sync":   `{"branch":"feat","base":"main","updated":true}`,
		"POST /check":      `{"passed":true,"checks":[]}`,
	}, seen)
	s := connect(t, ts.URL)

	res := call(t, s, "commit", map[string]any{"message": "do it", "dir": "/repo"})
	require.False(t, res.IsError, textOf(res))
	require.Contains(t, textOf(res), `"sha": "abc"`)
	require.Contains(t, seen["/git/commit"].body, `"message":"do it"`)

	res = call(t, s, "push", map[string]any{"dir": "/repo"})
	require.False(t, res.IsError, textOf(res))
	require.Contains(t, textOf(res), `"pushed": true`)

	res = call(t, s, "sync", map[string]any{"dir": "/repo", "base": "main"})
	require.False(t, res.IsError, textOf(res))
	require.Contains(t, textOf(res), `"updated": true`)
	require.Contains(t, seen["/git/sync"].body, `"base":"main"`)

	res = call(t, s, "check", map[string]any{"dir": "/repo", "name": "test"})
	require.False(t, res.IsError, textOf(res))
	require.Contains(t, textOf(res), `"passed": true`)
	require.Contains(t, seen["/check"].body, `"name":"test"`)
}

func TestGitToolSurfacesDaemonError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"protected branch"}`))
	}))
	defer ts.Close()
	s := connect(t, ts.URL)
	res := call(t, s, "commit", map[string]any{"dir": "/repo"})
	require.False(t, res.IsError, textOf(res))
	require.Contains(t, textOf(res), "error:")
	require.Contains(t, textOf(res), "protected branch")
}

func TestCtxTools(t *testing.T) {
	seen := map[string]recordedReq{}
	ts := fakeDaemon(t, map[string]string{
		"GET /context/k":         `{"key":"k","value":"the-value","updated_by":"agent"}`,
		"GET /context":           `{"entries":[{"key":"a","value":"1"}]}`,
		"PUT /context/k":         `{"key":"k","value":"v","updated_by":"agent"}`,
		"POST /context/k/append": `{"key":"k","value":"v1\nv2","updated_by":"agent"}`,
	}, seen)
	s := connect(t, ts.URL)

	res := call(t, s, "ctx_set", map[string]any{"key": "k", "value": "v"})
	require.False(t, res.IsError, textOf(res))
	require.Contains(t, textOf(res), "set k")

	res = call(t, s, "ctx_get", map[string]any{"key": "k"})
	require.False(t, res.IsError, textOf(res))
	require.Equal(t, "the-value", textOf(res))

	res = call(t, s, "ctx_list", map[string]any{"prefix": ""})
	require.False(t, res.IsError, textOf(res))
	require.Contains(t, textOf(res), `"key": "a"`)

	res = call(t, s, "ctx_append", map[string]any{"key": "k", "value": "v2"})
	require.False(t, res.IsError, textOf(res))
	require.Contains(t, textOf(res), "appended to k")
	// default separator is a newline when omitted
	require.Contains(t, seen["/context/k/append"].body, `"sep":"\n"`)
}

func TestCtxCASTool(t *testing.T) {
	seen := map[string]recordedReq{}
	ts := fakeDaemon(t, map[string]string{
		"POST /context/k/cas": `{"key":"k","value":"new","updated_by":"agent"}`,
	}, seen)
	s := connect(t, ts.URL)
	res := call(t, s, "ctx_cas", map[string]any{"key": "k", "expected": "old", "value": "new"})
	require.False(t, res.IsError, textOf(res))
	require.Contains(t, textOf(res), "set k")
	require.Contains(t, seen["/context/k/cas"].body, `"expected":"old"`)
}

func TestCtxCASToolConflict(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"changed"}`))
	}))
	defer ts.Close()
	s := connect(t, ts.URL)
	res := call(t, s, "ctx_cas", map[string]any{"key": "k", "value": "new"})
	require.False(t, res.IsError, textOf(res))
	require.Contains(t, textOf(res), "conflict")
}

func TestReadInboxTool(t *testing.T) {
	ts := fakeDaemon(t, map[string]string{
		"GET /sessions/A-1/messages": `{"messages":[{"id":"1","from":"x","body":"hi"}]}`,
	}, nil)
	s := connect(t, ts.URL)
	res := call(t, s, "read_inbox", map[string]any{"agent": "A-1"})
	require.False(t, res.IsError, textOf(res))
	require.Contains(t, textOf(res), "hi")
}

func TestReadInboxToolNeedsAgentID(t *testing.T) {
	// No agent arg and no WARDEN_SESSION_ID env => friendly error, no daemon call.
	t.Setenv("WARDEN_SESSION_ID", "")
	t.Setenv("AGENTCTL_SESSION_ID", "")
	ts := fakeDaemon(t, nil, nil)
	s := connect(t, ts.URL)
	res := call(t, s, "read_inbox", map[string]any{})
	require.False(t, res.IsError, textOf(res))
	require.Contains(t, textOf(res), "no agent id")
}

func TestWaitForMessageToolTimeout(t *testing.T) {
	ts := fakeDaemon(t, map[string]string{
		"GET /sessions/A-1/messages/wait": `{"found":false}`,
	}, nil)
	s := connect(t, ts.URL)
	res := call(t, s, "wait_for_message", map[string]any{"agent": "A-1", "timeout_sec": 1})
	require.False(t, res.IsError, textOf(res))
	require.Contains(t, textOf(res), "timed out")
}

func TestWaitForMessageToolDelivers(t *testing.T) {
	ts := fakeDaemon(t, map[string]string{
		"GET /sessions/A-1/messages/wait": `{"found":true,"message":{"id":"9","from":"B-2","body":"reply"}}`,
	}, nil)
	s := connect(t, ts.URL)
	res := call(t, s, "wait_for_message", map[string]any{"agent": "A-1", "timeout_sec": 1})
	require.False(t, res.IsError, textOf(res))
	require.Contains(t, textOf(res), "reply")
}

func TestCollaborationStatusTool(t *testing.T) {
	ts := fakeDaemon(t, map[string]string{
		"GET /collab/conflicts": `{"conflicts":[{"file":"a.go","agents":[{"id":"A-1"},{"id":"B-2"}]}]}`,
	}, nil)
	s := connect(t, ts.URL)
	res := call(t, s, "get_collaboration_status", map[string]any{})
	require.False(t, res.IsError, textOf(res))
	require.Contains(t, textOf(res), "a.go")
}

func TestBranchStatusTool(t *testing.T) {
	ts := fakeDaemon(t, map[string]string{
		"GET /collab/branches": `{"branches":[{"agent_id":"A-1","branch":"feat","ci":{"state":"success"}}]}`,
	}, nil)
	s := connect(t, ts.URL)
	res := call(t, s, "get_branch_status", map[string]any{})
	require.False(t, res.IsError, textOf(res))
	require.Contains(t, textOf(res), "success")
}

func TestWhoIsEditingFileTool(t *testing.T) {
	ts := fakeDaemon(t, map[string]string{
		"GET /collab/conflicts": `{"conflicts":[{"file":"a.go","agents":[{"id":"A-1"},{"id":"B-2"}]}]}`,
	}, nil)
	s := connect(t, ts.URL)

	// File with a conflict => returns the editing agents.
	res := call(t, s, "who_is_editing_file", map[string]any{"file": "a.go"})
	require.False(t, res.IsError, textOf(res))
	require.Contains(t, textOf(res), "A-1")

	// File with no conflict => the "no other agent" note.
	res = call(t, s, "who_is_editing_file", map[string]any{"file": "untouched.go"})
	require.False(t, res.IsError, textOf(res))
	require.Contains(t, textOf(res), "no other agent is editing untouched.go")
}

func TestListSchedulesTool(t *testing.T) {
	ts := fakeDaemon(t, map[string]string{
		"GET /schedules": `{"schedules":[{"id":"nightly","name":"nightly","kind":"cron"}]}`,
	}, nil)
	s := connect(t, ts.URL)
	res := call(t, s, "list_schedules", map[string]any{})
	require.False(t, res.IsError, textOf(res))
	require.Contains(t, textOf(res), "nightly")
}

func TestInsightsTool(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/sessions":
			_, _ = w.Write([]byte(`{"sessions":[]}`))
		case "/history":
			_, _ = w.Write([]byte(`{"sessions":[{"id":"A-1","status":"done"}]}`))
		case "/metrics/history":
			_, _ = w.Write([]byte(`{"summaries":[]}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer ts.Close()
	s := connect(t, ts.URL)
	res := call(t, s, "insights", map[string]any{"limit": 5})
	require.False(t, res.IsError, textOf(res))
	// The report is JSON; assert a known top-level section is present.
	require.Contains(t, textOf(res), "error_rates")
}

func TestSnapshotTools(t *testing.T) {
	seen := map[string]recordedReq{}
	ts := fakeDaemon(t, map[string]string{
		"POST /snapshots":                `{"id":"snap-1","branch":"feat"}`,
		"GET /snapshots":                 `{"snapshots":[{"id":"snap-1"}]}`,
		"POST /snapshots/snap-1/restore": `{"applied":true}`,
	}, seen)
	s := connect(t, ts.URL)

	res := call(t, s, "snapshot_create", map[string]any{"message": "good", "dir": "/repo"})
	require.False(t, res.IsError, textOf(res))
	require.Contains(t, textOf(res), "snap-1")
	require.Contains(t, seen["/snapshots"].body, `"message":"good"`)

	res = call(t, s, "snapshot_list", map[string]any{"all": true})
	require.False(t, res.IsError, textOf(res))
	require.Contains(t, textOf(res), "snap-1")

	res = call(t, s, "snapshot_restore", map[string]any{"id": "snap-1", "force": true})
	require.False(t, res.IsError, textOf(res))
	require.Contains(t, textOf(res), "applied")
	require.Contains(t, seen["/snapshots/snap-1/restore"].body, `"force":true`)
}

func TestFindApprovalErrors(t *testing.T) {
	views := []approval.View{
		{ID: "agent-1", Recognized: true, Options: []string{"Yes", "No"}, Fingerprint: "ff"},
		{ID: "agent-2", Recognized: false},
	}

	// Happy path.
	v, err := findApproval(views, "agent-1", 1)
	require.NoError(t, err)
	require.Equal(t, "ff", v.Fingerprint)

	// Option out of range.
	_, err = findApproval(views, "agent-1", 3)
	require.Error(t, err)
	require.Contains(t, err.Error(), "out of range")

	// Unrecognized menu.
	_, err = findApproval(views, "agent-2", 1)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not a recognized menu")

	// No such pending approval.
	_, err = findApproval(views, "ghost", 1)
	require.Error(t, err)
	require.Contains(t, err.Error(), "no pending approval")
}

func TestSessionIDAndCtxWriter(t *testing.T) {
	t.Setenv("WARDEN_SESSION_ID", "")
	t.Setenv("AGENTCTL_SESSION_ID", "")
	require.Equal(t, "", sessionID())
	require.Equal(t, "agent", ctxWriter(), "no session id falls back to generic writer")

	t.Setenv("AGENTCTL_SESSION_ID", "legacy-1")
	require.Equal(t, "legacy-1", sessionID(), "falls back to the legacy env var")

	t.Setenv("WARDEN_SESSION_ID", "A-1")
	require.Equal(t, "A-1", sessionID(), "prefers the canonical env var")
	require.Equal(t, "A-1", ctxWriter())
}
