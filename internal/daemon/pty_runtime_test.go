package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/poller"
	"github.com/srjn45/warden/internal/ptyevent"
	"github.com/srjn45/warden/internal/ptyhost"
	"github.com/srjn45/warden/internal/ptyruntime"
	"github.com/srjn45/warden/internal/store"
	"github.com/srjn45/warden/internal/terminalstore"
)

// ptyTmux runs tmux against an isolated server socket (never the user's).
type ptyTmux struct{ sock string }

func (r ptyTmux) Run(ctx context.Context, dir, name string, args ...string) (string, error) {
	if name == "tmux" {
		args = append([]string{"-L", r.sock, "-f", "/dev/null"}, args...)
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "TMUX=") && !strings.HasPrefix(e, "SHELL=") {
			env = append(env, e)
		}
	}
	cmd.Env = append(env, "SHELL=/bin/sh")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func ptyServer(t *testing.T, mode ptyhost.Mode) (*Server, *ptyhost.Manager, *httptest.Server, *fakeStore) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	run := ptyTmux{sock: fmt.Sprintf("wd-daemon-pty-%d-%d", os.Getpid(), time.Now().UnixNano())}
	t.Cleanup(func() { _, _ = run.Run(context.Background(), "", "tmux", "kill-server") })
	m := ptyhost.New(ptyhost.Config{Mode: mode, Dir: filepath.Join(t.TempDir(), "pty"), Run: run, Shell: []string{"/bin/sh"},
		Options: ptyruntime.Options{IdleFlush: 20 * time.Millisecond, KillGrace: 300 * time.Millisecond, DrainGrace: 200 * time.Millisecond}})
	fs := newFakeStore()
	srv := &Server{store: fs, life: &fakeLife{}}
	srv.SetPTYRuntime(m)
	ts := httptest.NewServer(srv.router())
	t.Cleanup(func() { ts.Close(); m.Close() })
	return srv, m, ts, fs
}

func TestPTYStreamUnknownSessionIs404(t *testing.T) {
	ts := lifeServer(t, newFakeStore(), &fakeLife{})
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/api/v1/sessions/nope/pty")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestPTYStreamWithoutRuntimeIs409(t *testing.T) {
	fs := newFakeStore()
	_ = fs.Insert(context.Background(), &agentstore.Agent{ID: "A-1", TmuxSession: "A-1"})
	ts := lifeServer(t, fs, &fakeLife{})
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/api/v1/sessions/A-1/pty")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusConflict, resp.StatusCode)
}

func TestPTYStreamRouteClassification(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/A-1/pty", nil)
	require.True(t, isStreamingPath(r), "the WS stream must bypass the write timeout")
	require.False(t, isWriteRequest(r), "a read-only token may observe the stream (input is refused in-handler)")
}

// Off mode leaves the session on tmux with no journal: the stream route says
// so instead of upgrading.
func TestPTYStreamOffModeIs409(t *testing.T) {
	_, m, ts, fs := ptyServer(t, ptyhost.ModeOff)
	require.NoError(t, m.Host().NewSession(context.Background(), "A-1", t.TempDir()))
	_ = fs.Insert(context.Background(), &agentstore.Agent{ID: "A-1", TmuxSession: "A-1"})
	resp, err := http.Get(ts.URL + "/api/v1/sessions/A-1/pty")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusConflict, resp.StatusCode)
}

// Shadow mode end to end through the daemon route: hello → plan → mirrored
// events of an unchanged tmux pane.
func TestPTYStreamShadowSessionThroughDaemon(t *testing.T) {
	_, m, ts, fs := ptyServer(t, ptyhost.ModeShadow)
	ctx := context.Background()
	require.NoError(t, m.Host().NewSession(ctx, "A-1", t.TempDir()))
	_ = fs.Insert(ctx, &agentstore.Agent{ID: "A-1", TmuxSession: "A-1"})
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/v1/sessions/A-1/pty", nil)
	require.NoError(t, err)
	defer conn.CloseNow()
	hello, _ := json.Marshal(ptyhost.ClientFrame{Type: "hello", Mode: ptyhost.ModeInteractive})
	require.NoError(t, conn.Write(ctx, websocket.MessageText, hello))
	require.NoError(t, m.Host().SendKeys(ctx, "A-1", "printf 'via-%s\\n' daemon", "Enter"))

	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var plan *ptyhost.PlanFrame
	var out strings.Builder
	for !strings.Contains(out.String(), "via-daemon") {
		typ, data, err := conn.Read(rctx)
		require.NoError(t, err)
		require.Equal(t, websocket.MessageText, typ, "a shadow stream is observe-only: no raw frames")
		var f ptyhost.ServerFrame
		require.NoError(t, json.Unmarshal(data, &f))
		switch f.Type {
		case "plan":
			plan = f.Plan
		case "event":
			if f.Event.Kind == ptyevent.KindOutput {
				out.Write(f.Event.Output.Data)
			}
		}
	}
	require.NotNil(t, plan)
	require.False(t, plan.Interactive)
	require.Equal(t, ptyevent.PresentationTmux, plan.Presentation)
}

// A direct terminal (mode on) has no tmux session: input, output and the
// legacy raw attach all route to the PTY runtime.
func TestDirectTerminalInputOutputAndLegacyAttach(t *testing.T) {
	_, m, ts, fs := ptyServer(t, ptyhost.ModeOn)
	ctx := context.Background()
	require.NoError(t, m.TerminalHost().NewSession(ctx, "T-1", t.TempDir()))
	_ = fs.Insert(ctx, &agentstore.Agent{ID: "T-1", TmuxSession: "T-1"})

	body, _ := json.Marshal(map[string]string{"text": "echo hi-$((40+2))"})
	resp, err := http.Post(ts.URL+"/api/v1/sessions/T-1/input", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Eventually(t, func() bool {
		resp, err := http.Get(ts.URL + "/api/v1/sessions/T-1/output")
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return strings.Contains(string(b), "hi-42")
	}, 10*time.Second, 50*time.Millisecond)

	// Legacy /attach: raw bytes in and out, {cols,rows} resize.
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/v1/sessions/T-1/attach", nil)
	require.NoError(t, err)
	defer conn.CloseNow()
	require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(`{"cols":91,"rows":27}`)))
	require.NoError(t, conn.Write(ctx, websocket.MessageBinary, []byte("stty size\r")))
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var got strings.Builder
	for !strings.Contains(got.String(), "27 91") {
		_, data, err := conn.Read(rctx)
		require.NoError(t, err, "got so far: %q", got.String())
		got.Write(data)
	}
}

// When a direct terminal's shell exits, the stream consumer and the terminal
// watcher both see it; the shared compare-and-swap means exactly one
// running → orphaned transition, whoever wins.
func TestDirectTerminalExitDoesNotFightTheWatcher(t *testing.T) {
	srv, m, _, _ := ptyServer(t, ptyhost.ModeOn)
	ctx := context.Background()
	terms, err := terminalstore.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = terms.Close() })
	srv.SetTerminals(terms)
	require.NoError(t, m.TerminalHost().NewSession(ctx, "T-1", t.TempDir()))
	now := time.Now().UTC()
	require.NoError(t, terms.Spawn(ctx, &terminalstore.Terminal{ID: "T-1", TmuxSession: "T-1", Status: terminalstore.StatusRunning,
		CreatedAt: now, UpdatedAt: now}, "T-1"))

	tw := poller.NewTerminalWatcher(NewTerminalPollerDeps(terms, m.Host(), nil))
	var mu sync.Mutex
	transitions := 0
	tw.OnTransition = func(_ *store.Session, _, to store.Status) {
		if to == store.StatusOrphaned {
			mu.Lock()
			transitions++
			mu.Unlock()
		}
	}
	srv.SetTerminalWatcher(tw)
	wctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); tw.Run(wctx, 10*time.Millisecond) }()
	t.Cleanup(func() { cancel(); <-done })

	require.NoError(t, m.Host().SendKeys(ctx, "T-1", "exit", "Enter"))
	require.Eventually(t, func() bool {
		got, err := terms.Get(ctx, "T-1")
		return err == nil && got.Status == terminalstore.StatusOrphaned
	}, 10*time.Second, 20*time.Millisecond)
	time.Sleep(200 * time.Millisecond) // let the slower observer land too
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, 1, transitions, "exactly one orphaned transition")
}

func TestPTYRuntimeWiresClassifierAndPollerAuthoritative(t *testing.T) {
	srv, m, _, fs := ptyServer(t, ptyhost.ModeOn)
	pl := poller.New(nil, time.Minute)
	srv.poller = pl
	srv.SetPTYRuntime(m)

	require.NotNil(t, srv.Classifier(), "classifier must be instantiated")
	require.NotNil(t, pl.Authoritative, "poller Authoritative guard must be set")

	ctx := context.Background()
	require.NoError(t, m.TerminalHost().NewSession(ctx, "T-1", t.TempDir()))
	_ = fs.Insert(ctx, &agentstore.Agent{ID: "T-1", TmuxSession: "T-1"})

	// ModeOn makes T-1 authoritative.
	require.True(t, pl.Authoritative("T-1"))

	// OnPTYEvent dispatches to classifier.
	srv.onPTYEvent("T-1", ptyevent.Event{
		Kind: ptyevent.KindLifecycle,
		Lifecycle: &ptyevent.Lifecycle{
			Phase: ptyevent.PhaseSpawned,
			PID:   1234,
		},
	})
	snap, ok := srv.Classifier().CurrentState("T-1")
	require.True(t, ok)
	require.Equal(t, ptyevent.StateWaiting, snap.State)
	require.Equal(t, ptyevent.ReasonStartup, snap.Reason)
}
