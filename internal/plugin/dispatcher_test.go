package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/store"
)

// regWith builds a registry of plugins each subscribed to one event, bypassing
// Load's path validation so tests can use fake paths.
func regWith(plugins ...Plugin) *Registry {
	return &Registry{plugins: plugins, customTypes: map[string]store.CustomTypePolicy{}}
}

func TestDispatchNilSafe(t *testing.T) {
	var d *Dispatcher
	require.NotPanics(t, func() {
		d.Dispatch(context.Background(), EventPreSpawn, SessionMeta{}, nil)
	})
}

func TestDispatchNoRegistryNoSubscribers(t *testing.T) {
	d := NewDispatcher(regWith(Plugin{Name: "p", Path: "/x", Events: []HookEvent{EventPostCommit}}))
	called := false
	d.runner = func(ctx context.Context, path string, stdin []byte) ([]byte, error) {
		called = true
		return nil, nil
	}
	// No plugin subscribes to pre-spawn → runner never invoked.
	d.Dispatch(context.Background(), EventPreSpawn, SessionMeta{}, nil)
	require.False(t, called)
}

func TestDispatchHappyPath(t *testing.T) {
	d := NewDispatcher(regWith(Plugin{Name: "ok", Path: "/bin/ok", Events: []HookEvent{EventPostCommit}}))
	var gotPath string
	var gotReq Request
	d.runner = func(ctx context.Context, path string, stdin []byte) ([]byte, error) {
		gotPath = path
		require.NoError(t, json.Unmarshal(stdin, &gotReq))
		out, _ := json.Marshal(Response{ProtocolVersion: ProtocolVersion, OK: true, Message: "noted"})
		return out, nil
	}
	d.Dispatch(context.Background(), EventPostCommit,
		SessionMeta{ID: "dev-1", Type: "development"},
		map[string]string{"sha": "abc"})

	require.Equal(t, "/bin/ok", gotPath)
	require.Equal(t, EventPostCommit, gotReq.Event)
	require.Equal(t, ProtocolVersion, gotReq.ProtocolVersion)
	require.Equal(t, "dev-1", gotReq.Session.ID)
	require.Equal(t, "abc", gotReq.Payload["sha"])
}

// TestDispatchFailOpen proves that none of the failure modes — error/non-zero
// exit, malformed JSON, empty output, ok:false — panic or abort the dispatch
// loop: a healthy plugin registered after a failing one still runs.
func TestDispatchFailOpen(t *testing.T) {
	failModes := map[string]func(stdin []byte) ([]byte, error){
		"runner error (missing binary / non-zero exit)": func([]byte) ([]byte, error) {
			return nil, errors.New("exec: no such file")
		},
		"malformed json": func([]byte) ([]byte, error) {
			return []byte("not json{"), nil
		},
		"empty output": func([]byte) ([]byte, error) {
			return nil, nil
		},
		"ok false": func([]byte) ([]byte, error) {
			out, _ := json.Marshal(Response{ProtocolVersion: ProtocolVersion, OK: false, Message: "vetoed but advisory"})
			return out, nil
		},
		"version mismatch": func([]byte) ([]byte, error) {
			out, _ := json.Marshal(Response{ProtocolVersion: 999, OK: true})
			return out, nil
		},
	}
	for name, broken := range failModes {
		t.Run(name, func(t *testing.T) {
			d := NewDispatcher(regWith(
				Plugin{Name: "broken", Path: "/b", Events: []HookEvent{EventPreCommit}},
				Plugin{Name: "healthy", Path: "/h", Events: []HookEvent{EventPreCommit}},
			))
			healthyRan := false
			d.runner = func(ctx context.Context, path string, stdin []byte) ([]byte, error) {
				if path == "/b" {
					return broken(stdin)
				}
				healthyRan = true
				out, _ := json.Marshal(Response{ProtocolVersion: ProtocolVersion, OK: true})
				return out, nil
			}
			require.NotPanics(t, func() {
				d.Dispatch(context.Background(), EventPreCommit, SessionMeta{}, nil)
			})
			require.True(t, healthyRan, "a failing plugin must not abort the dispatch loop")
		})
	}
}

// TestExecRunnerHappyPath exercises the REAL subprocess path against a tiny
// stub plugin script: it must receive the request on stdin and its stdout
// response must be parsed without error.
func TestExecRunnerHappyPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh stub not portable to windows")
	}
	dir := t.TempDir()
	// The stub echoes a valid response and records the request it received.
	gotFile := filepath.Join(dir, "got.json")
	script := "#!/bin/sh\ncat > " + gotFile + "\nprintf '{\"protocol_version\":1,\"ok\":true,\"message\":\"hi\"}'\n"
	path := writeScript(t, dir, "plugin.sh", script)

	d := NewDispatcher(regWith(Plugin{Name: "stub", Path: path, Events: []HookEvent{EventPostSpawn}}))
	require.NotPanics(t, func() {
		d.Dispatch(context.Background(), EventPostSpawn, SessionMeta{ID: "agent-x", Type: "development"}, nil)
	})

	raw, err := os.ReadFile(gotFile)
	require.NoError(t, err)
	var req Request
	require.NoError(t, json.Unmarshal(raw, &req))
	require.Equal(t, EventPostSpawn, req.Event)
	require.Equal(t, "agent-x", req.Session.ID)
}

// TestExecRunnerMissingBinaryFailsOpen confirms the real runner returns an error
// (not a panic) for a non-existent executable, which Dispatch swallows.
func TestExecRunnerMissingBinaryFailsOpen(t *testing.T) {
	d := NewDispatcher(regWith(Plugin{Name: "ghost", Path: "/no/such/binary/at/all", Events: []HookEvent{EventPostSpawn}}))
	require.NotPanics(t, func() {
		d.Dispatch(context.Background(), EventPostSpawn, SessionMeta{}, nil)
	})
}

// TestExecRunnerTimeout confirms a slow plugin is killed by the hard timeout and
// the dispatch returns promptly rather than hanging.
func TestExecRunnerTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh stub not portable to windows")
	}
	dir := t.TempDir()
	path := writeScript(t, dir, "slow.sh", "#!/bin/sh\nsleep 30\n")

	d := NewDispatcher(regWith(Plugin{Name: "slow", Path: path, Events: []HookEvent{EventPreCheck}}))
	d.timeout = 100 * time.Millisecond

	done := make(chan struct{})
	go func() {
		d.Dispatch(context.Background(), EventPreCheck, SessionMeta{}, nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("dispatch did not return — timeout did not kill the slow plugin")
	}
}

// TestDispatchConcurrentSafe runs many dispatches in parallel to catch any data
// race in the fail-open path (run with -race).
func TestDispatchConcurrentSafe(t *testing.T) {
	d := NewDispatcher(regWith(Plugin{Name: "p", Path: "/x", Events: []HookEvent{EventPostCommit}}))
	d.runner = func(ctx context.Context, path string, stdin []byte) ([]byte, error) {
		out, _ := json.Marshal(Response{ProtocolVersion: ProtocolVersion, OK: true})
		return out, nil
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.Dispatch(context.Background(), EventPostCommit, SessionMeta{ID: "x"}, nil)
		}()
	}
	wg.Wait()
}

func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(p, []byte(body), 0o755))
	return p
}
