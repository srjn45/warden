package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestIsConnRefused(t *testing.T) {
	require.True(t, isConnRefused(syscall.ECONNREFUSED))
	require.False(t, isConnRefused(errors.New("some other transport error")),
		"non-refused errors must not be reported as a down daemon")
}

func TestListSessions(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/sessions", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"sessions":[{"id":"A-1","status":"working"}]}`))
	}))
	defer ts.Close()

	c := New(ts.URL)
	out, err := c.List(t.Context())
	require.NoError(t, err)
	require.Len(t, out, 1)
	require.Equal(t, "A-1", out[0].ID)
}

func TestDaemonDownGivesFriendlyError(t *testing.T) {
	c := New("http://127.0.0.1:1") // nothing listening
	_, err := c.List(t.Context())
	require.Error(t, err)
	require.ErrorIs(t, err, ErrDaemonDown)
}

func TestSpawn(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/spawn", r.URL.Path)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"A-1","status":"spawning"}`))
	}))
	defer ts.Close()
	s, err := New(ts.URL).Spawn(t.Context(), SpawnParams{Type: "development", Ticket: "A-1", Repo: "/repo"})
	require.NoError(t, err)
	require.Equal(t, "A-1", s.ID)
}

func TestLongOperationsOutlastShortTimeout(t *testing.T) {
	// Reads use a short default deadline; long operations (spawn/adopt/
	// remove-worktree) get a generous one. A slow-but-successful spawn must not
	// be aborted by the short read timeout (the old blanket 10s client timeout).
	origDefault, origLong := defaultTimeout, longTimeout
	defaultTimeout, longTimeout = 20*time.Millisecond, 2*time.Second
	defer func() { defaultTimeout, longTimeout = origDefault, origLong }()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(120 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"A-1","status":"spawning"}`))
	}))
	defer ts.Close()
	c := New(ts.URL)

	_, err := c.List(context.Background())
	require.Error(t, err, "a read should hit the short default timeout against a 120ms server")

	s, err := c.Spawn(context.Background(), SpawnParams{Prompt: "hi", Cwd: "/tmp"})
	require.NoError(t, err, "spawn should outlast the short read timeout")
	require.Equal(t, "A-1", s.ID)
}

func TestCallerDeadlineIsNotOverridden(t *testing.T) {
	// When the caller supplies its own deadline, the client must respect it and
	// not silently extend it to the long-operation timeout.
	origLong := longTimeout
	longTimeout = 5 * time.Second
	defer func() { longTimeout = origLong }()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(120 * time.Millisecond)
		_, _ = w.Write([]byte(`{"id":"A-1"}`))
	}))
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()
	_, err := New(ts.URL).Spawn(ctx, SpawnParams{Prompt: "hi", Cwd: "/tmp"})
	require.Error(t, err, "caller's 15ms deadline must apply even to a long operation")
}

func TestListDirs(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/fs/dirs", r.URL.Path)
		require.Equal(t, "/home/me/work", r.URL.Query().Get("path"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"path":"/home/me/work","parent":"/home/me","entries":[{"name":"api","path":"/home/me/work/api"}]}`))
	}))
	defer ts.Close()

	l, err := New(ts.URL).ListDirs(t.Context(), "/home/me/work")
	require.NoError(t, err)
	require.Equal(t, "/home/me/work", l.Path)
	require.Equal(t, "/home/me", l.Parent)
	require.Len(t, l.Entries, 1)
	require.Equal(t, "api", l.Entries[0].Name)
	require.Equal(t, "/home/me/work/api", l.Entries[0].Path)
}

func TestRemoveWorktreeConflictIsStatusError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"uncommitted changes"}`))
	}))
	defer ts.Close()
	err := New(ts.URL).RemoveWorktree(t.Context(), "A-1", false)
	require.Error(t, err)
	var se *StatusError
	require.ErrorAs(t, err, &se)
	require.Equal(t, 409, se.Code)
}

func TestClientApprovals(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/approvals", r.URL.Path)
		w.Write([]byte(`{"enabled":true,"approvals":[{"id":"a1","recognized":true,"options":["Yes","No"],"fingerprint":"ff"}]}`))
	}))
	defer ts.Close()
	c := New(ts.URL)
	enabled, views, err := c.Approvals(context.Background())
	require.NoError(t, err)
	require.True(t, enabled)
	require.Len(t, views, 1)
	require.Equal(t, "a1", views[0].ID)
}

func TestClientApprove(t *testing.T) {
	var gotBody map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/sessions/a1/approve", r.URL.Path)
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Write([]byte(`{"status":"answered"}`))
	}))
	defer ts.Close()
	c := New(ts.URL)
	require.NoError(t, c.Approve(context.Background(), "a1", 2, "ff"))
	require.Equal(t, float64(2), gotBody["option"])
	require.Equal(t, "ff", gotBody["fingerprint"])
}

func TestAdoptSendsBodyAndParsesResponse(t *testing.T) {
	var gotBody map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/adopt", r.URL.Path)
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"session":{"id":"agent-x"},"warning":"heads up"}`))
	}))
	defer ts.Close()

	res, err := New(ts.URL).Adopt(t.Context(), AdoptParams{
		Cwd: "/tmp/p", SessionID: "sid", TmuxSession: "work",
	})
	require.NoError(t, err)
	require.Equal(t, "agent-x", res.Session.ID)
	require.Equal(t, "heads up", res.Warning)
	require.Equal(t, "/tmp/p", gotBody["cwd"])
	require.Equal(t, "sid", gotBody["session_id"])
	require.Equal(t, "work", gotBody["tmux_session"])
}

func TestClientSpawnSendsSupervised(t *testing.T) {
	var got map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(`{"id":"a1"}`))
	}))
	defer ts.Close()
	_, err := New(ts.URL).Spawn(context.Background(), SpawnParams{Prompt: "x", Supervised: true})
	require.NoError(t, err)
	require.Equal(t, true, got["supervised"])
}

func TestCtxSetSendsValueAndBy(t *testing.T) {
	var gotPath, gotMethod, gotBody string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"key":"global.k","value":"v","updated_by":"agent-A"}`))
	}))
	defer ts.Close()

	e, err := New(ts.URL).CtxSet(context.Background(), "global.k", "v", "agent-A")
	if err != nil {
		t.Fatalf("CtxSet: %v", err)
	}
	if gotMethod != http.MethodPut || gotPath != "/context/global.k" {
		t.Fatalf("got %s %s", gotMethod, gotPath)
	}
	if !strings.Contains(gotBody, `"value":"v"`) || !strings.Contains(gotBody, `"by":"agent-A"`) {
		t.Fatalf("body=%s", gotBody)
	}
	if e.UpdatedBy != "agent-A" {
		t.Fatalf("entry=%+v", e)
	}
}

func TestCtxList(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("prefix") != "pipeline." {
			t.Errorf("prefix not forwarded: %q", r.URL.Query().Get("prefix"))
		}
		w.Write([]byte(`{"entries":[{"key":"pipeline.p.a","value":"A"}]}`))
	}))
	defer ts.Close()

	got, err := New(ts.URL).CtxList(context.Background(), "pipeline.")
	if err != nil {
		t.Fatalf("CtxList: %v", err)
	}
	if len(got) != 1 || got[0].Key != "pipeline.p.a" {
		t.Fatalf("got %+v", got)
	}
}

func TestCtxSetRejectsSlashKeyBeforeCall(t *testing.T) {
	called := false
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer ts.Close()

	if _, err := New(ts.URL).CtxSet(context.Background(), "bad/key", "v", "by"); err == nil {
		t.Fatalf("expected error for slash key")
	}
	if err := New(ts.URL).CtxDel(context.Background(), "a\\b"); err == nil {
		t.Fatalf("expected error for backslash key")
	}
	if called {
		t.Fatalf("client must reject invalid keys before calling the daemon")
	}
}

func TestMsgSendParsesMessageAndWoke(t *testing.T) {
	var gotPath, gotMethod, gotBody string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Write([]byte(`{"message":{"id":"1","from":"agent-2","to":"agent-1","body":"hi"},"woke":true}`))
	}))
	defer ts.Close()

	m, woke, err := New(ts.URL).MsgSend(context.Background(), "agent-1", "agent-2", "hi")
	if err != nil {
		t.Fatalf("MsgSend: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/sessions/agent-1/messages" {
		t.Fatalf("got %s %s", gotMethod, gotPath)
	}
	if !strings.Contains(gotBody, `"from":"agent-2"`) || !strings.Contains(gotBody, `"body":"hi"`) {
		t.Fatalf("body=%s", gotBody)
	}
	if m.ID != "1" || !woke {
		t.Fatalf("m=%+v woke=%v", m, woke)
	}
}

func TestMsgInboxForwardsUnread(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("unread") != "true" {
			t.Errorf("unread not forwarded")
		}
		w.Write([]byte(`{"messages":[{"id":"1","from":"x","body":"a"}]}`))
	}))
	defer ts.Close()

	got, err := New(ts.URL).MsgInbox(context.Background(), "agent-1", true)
	if err != nil {
		t.Fatalf("MsgInbox: %v", err)
	}
	if len(got) != 1 || got[0].Body != "a" {
		t.Fatalf("got %+v", got)
	}
}

func TestMsgWaitFoundAndTimeout(t *testing.T) {
	// found
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("timeout") != "1" || r.URL.Query().Get("from") != "agent-2" {
			t.Errorf("query not forwarded: %s", r.URL.RawQuery)
		}
		w.Write([]byte(`{"found":true,"message":{"id":"1","from":"agent-2","body":"reply"}}`))
	}))
	m, err := New(ts.URL).MsgWait(context.Background(), "agent-1", "agent-2", 1)
	ts.Close()
	if err != nil || m == nil || m.Body != "reply" {
		t.Fatalf("found case: m=%+v err=%v", m, err)
	}

	// timeout
	ts2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"found":false}`))
	}))
	defer ts2.Close()
	m2, err := New(ts2.URL).MsgWait(context.Background(), "agent-1", "", 1)
	if err != nil || m2 != nil {
		t.Fatalf("timeout case: m=%+v err=%v", m2, err)
	}
}
