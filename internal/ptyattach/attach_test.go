//go:build linux || darwin

package ptyattach

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/creack/pty"
	"golang.org/x/sys/unix"

	"github.com/srjn45/warden/internal/ptyhost"
	"github.com/srjn45/warden/internal/ptyruntime"
)

// safeBuf is a goroutine-safe sink for what the client renders.
type safeBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *safeBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	end := time.Now().Add(10 * time.Second)
	for time.Now().Before(end) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// rig is a daemon (ptyhost.Manager + /pty route) with one direct shell.
type rig struct {
	m    *ptyhost.Manager
	url  string
	srv  *httptest.Server
	down atomic.Bool
}

func newRig(t *testing.T, shell ...string) *rig {
	t.Helper()
	if len(shell) == 0 {
		shell = []string{"/bin/sh"}
	}
	m := ptyhost.New(ptyhost.Config{Mode: ptyhost.ModeOn, Dir: filepath.Join(t.TempDir(), "pty"), Shell: shell,
		Options: ptyruntime.Options{IdleFlush: 20 * time.Millisecond, KillGrace: 300 * time.Millisecond}})
	t.Cleanup(m.Close)
	if err := m.TerminalHost().NewSession(context.Background(), "term-a", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	rg := &rig{m: m}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rg.down.Load() {
			http.Error(w, "daemon down", http.StatusServiceUnavailable)
			return
		}
		tg, ok := m.Lookup("term-a")
		if !ok {
			http.Error(w, "no journal", http.StatusConflict)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		_ = m.Serve(r.Context(), conn, tg, "sock")
	}))
	t.Cleanup(srv.Close)
	rg.srv, rg.url = srv, "ws"+strings.TrimPrefix(srv.URL, "http")
	return rg
}

// term is a fake local terminal: the program under test reads/writes the slave
// side (a real tty, so raw mode is real); the "user" types on the master side.
type term struct {
	user  *os.File // master: what the user types / what the terminal displays
	tty   *os.File // slave: Options.In
	shown safeBuf
}

func newTerm(t *testing.T, cols, rows uint16) *term {
	t.Helper()
	m, s, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	_ = pty.Setsize(m, &pty.Winsize{Cols: cols, Rows: rows})
	tm := &term{user: m, tty: s}
	t.Cleanup(func() { m.Close(); s.Close() })
	go func() { // drain what the "terminal" displays so the tty never blocks
		buf := make([]byte, 4096)
		for {
			n, err := m.Read(buf)
			if n > 0 {
				tm.shown.Write(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	return tm
}

func (tm *term) type_(s string) { _, _ = tm.user.WriteString(s) }

func (tm *term) isRaw(t *testing.T) bool {
	t.Helper()
	tio, err := unix.IoctlGetTermios(int(tm.tty.Fd()), ioctlGet)
	if err != nil {
		t.Fatal(err)
	}
	return tio.Lflag&(unix.ICANON|unix.ECHO|unix.ISIG) == 0
}

type runResult struct {
	res Result
	err error
}

func start(t *testing.T, r *rig, tm *term, mut func(*Options)) (*safeBuf, <-chan runResult, context.CancelFunc) {
	t.Helper()
	out := &safeBuf{}
	o := Options{URL: r.url, In: tm.tty, Out: out, ClientID: "cli", Takeover: true}
	if mut != nil {
		mut(&o)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan runResult, 1)
	go func() {
		res, err := Run(ctx, o)
		done <- runResult{res, err}
	}()
	t.Cleanup(cancel)
	return out, done, cancel
}

func wait(t *testing.T, done <-chan runResult) runResult {
	t.Helper()
	select {
	case r := <-done:
		return r
	case <-time.After(10 * time.Second):
		t.Fatal("attach did not return")
		return runResult{}
	}
}

// F-ATTACH-01: raw mode is entered and restored on every way out.
func TestF_ATTACH_01_RawModeRestoredOnEveryExit(t *testing.T) {
	cases := []struct {
		name string
		end  func(t *testing.T, r *rig, tm *term, cancel context.CancelFunc)
		want string
	}{
		{"detach key", func(t *testing.T, r *rig, tm *term, cancel context.CancelFunc) { tm.type_("\x1d.") }, "detached"},
		{"process exits", func(t *testing.T, r *rig, tm *term, cancel context.CancelFunc) { tm.type_("exit 3\r") }, "exited"},
		{"context cancelled", func(t *testing.T, r *rig, tm *term, cancel context.CancelFunc) { cancel() }, "detached"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			tm := newTerm(t, 100, 30)
			if tm.isRaw(t) {
				t.Fatal("test terminal starts raw")
			}
			out, done, cancel := start(t, r, tm, nil)
			eventually(t, "terminal to go raw", func() bool { return tm.isRaw(t) })
			tm.type_("echo hello-attach\r")
			eventually(t, "shell output", func() bool { return strings.Contains(out.String(), "hello-attach") })
			c.end(t, r, tm, cancel)
			res := wait(t, done)
			if res.err != nil || res.res.Reason != c.want {
				t.Fatalf("result = %+v, want reason %q", res, c.want)
			}
			if c.want == "exited" && (res.res.ExitCode == nil || *res.res.ExitCode != 3) {
				t.Fatalf("exit code = %v, want 3", res.res.ExitCode)
			}
			if tm.isRaw(t) {
				t.Fatal("terminal left in raw mode")
			}
		})
	}
	t.Run("daemon unreachable", func(t *testing.T) {
		r := newRig(t)
		tm := newTerm(t, 80, 24)
		var mu sync.Mutex
		var first *websocket.Conn
		_, done, _ := start(t, r, tm, func(o *Options) {
			o.MaxReconnects = 1
			o.Backoff = func(int) time.Duration { return time.Millisecond }
			o.Dial = func(ctx context.Context, url string, h http.Header) (*websocket.Conn, error) {
				c, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: h})
				mu.Lock()
				if err == nil && first == nil {
					first = c
				}
				mu.Unlock()
				return c, err
			}
		})
		eventually(t, "terminal to go raw", func() bool { return tm.isRaw(t) })
		r.down.Store(true) // the daemon is gone: every re-dial is refused
		mu.Lock()
		_ = first.CloseNow()
		mu.Unlock()
		res := wait(t, done)
		if res.err == nil {
			t.Fatalf("expected an error after the daemon went away, got %+v", res)
		}
		if tm.isRaw(t) {
			t.Fatal("terminal left in raw mode after the daemon vanished")
		}
	})
}

// Ctrl-C and Ctrl-D reach the REMOTE line discipline as bytes (the local tty is
// raw): Ctrl-C interrupts the foreground job but not the shell, Ctrl-D at an
// empty prompt is EOF and ends the shell with status 0.
func TestCtrlCAndEOFArePassedToTheRemotePTY(t *testing.T) {
	r := newRig(t)
	tm := newTerm(t, 100, 30)
	out, done, _ := start(t, r, tm, nil)
	eventually(t, "raw", func() bool { return tm.isRaw(t) })
	tm.type_("sleep 30\r")
	time.Sleep(100 * time.Millisecond) // let sleep start; nothing observable to wait on
	tm.type_("\x03")                   // Ctrl-C
	tm.type_("echo rc=$?\r")
	// 130 = 128+SIGINT: the foreground job was interrupted, the shell lived on.
	eventually(t, "the job to die of SIGINT and the shell to survive", func() bool { return strings.Contains(out.String(), "rc=130") })
	tm.type_("\x04") // Ctrl-D at an empty prompt
	res := wait(t, done)
	if res.err != nil || res.res.Reason != "exited" || res.res.ExitCode == nil || *res.res.ExitCode != 0 {
		t.Fatalf("EOF result = %+v", res)
	}
}

// Job control belongs to the remote PTY: Ctrl-Z stops the foreground job and
// the shell regains the terminal (process groups behave natively).
func TestJobControlOverAttach(t *testing.T) {
	r := newRig(t)
	tm := newTerm(t, 100, 30)
	out, done, cancel := start(t, r, tm, nil)
	eventually(t, "raw", func() bool { return tm.isRaw(t) })
	tm.type_("sleep 30\r")
	time.Sleep(100 * time.Millisecond)
	tm.type_("\x1a") // Ctrl-Z
	eventually(t, "the shell to report the stopped job", func() bool {
		return strings.Contains(out.String(), "Stopped") || strings.Contains(out.String(), "stopped")
	})
	tm.type_("echo prompt-is-back; kill %1\r")
	eventually(t, "the prompt after Ctrl-Z", func() bool { return strings.Contains(out.String(), "prompt-is-back") })
	cancel()
	wait(t, done)
}

// Resize: the local terminal's size reaches the remote PTY (SIGWINCH) and the
// client keeps following it.
func TestResizeFollowsLocalTerminal(t *testing.T) {
	r := newRig(t)
	tm := newTerm(t, 100, 30)
	winch := make(chan struct{}, 1)
	out, done, cancel := start(t, r, tm, func(o *Options) { o.Winch = winch })
	eventually(t, "raw", func() bool { return tm.isRaw(t) })
	tm.type_("stty size\r")
	eventually(t, "initial size", func() bool { return strings.Contains(out.String(), "30 100") })
	_ = pty.Setsize(tm.user, &pty.Winsize{Cols: 132, Rows: 43})
	winch <- struct{}{}
	// The writer applies the resize only after a frame wakes it; type to prove it.
	eventually(t, "resized remote PTY", func() bool {
		tm.type_("stty size\r")
		time.Sleep(20 * time.Millisecond)
		return strings.Contains(out.String(), "43 132")
	})
	cancel()
	wait(t, done)
}

// UTF-8, wide characters, SGR colors, cursor addressing and the alternate
// screen of a full-screen program reach the local terminal byte-for-byte: the
// live feed is the program's own output, never a re-rendering.
func TestFaithfulBytesForUTF8ANSIAndFullscreen(t *testing.T) {
	r := newRig(t)
	tm := newTerm(t, 100, 30)
	out, done, cancel := start(t, r, tm, nil)
	eventually(t, "raw", func() bool { return tm.isRaw(t) })
	// A tiny "vim": enter the alt screen, position, color, wide + combining +
	// emoji text, then leave the alt screen.
	tm.type_(`printf '\033[?1049h\033[2J\033[5;10H\033[1;38;5;196mXX\033[0m \344\275\240\345\245\275 e\314\201 \360\237\230\200\033[?1049l\n'` + "\r")
	want := "\x1b[?1049h\x1b[2J\x1b[5;10H\x1b[1;38;5;196mXX\x1b[0m 你好 e\u0301 😀\x1b[?1049l"
	eventually(t, "the exact byte sequence", func() bool { return strings.Contains(out.String(), want) })
	cancel()
	wait(t, done)
}

// A client that connects while output is flowing sees every line once, in
// order: the replay (journal) and the live feed meet at a quiescent cut.
func TestReplayThenLiveOrderingMidStream(t *testing.T) {
	r := newRig(t)
	// Output already in the journal, then a producer still running at attach.
	_ = r.m.Host().SendKeys(context.Background(), "term-a", "-l",
		`i=1; while [ $i -le 150 ]; do echo "L$i"; i=$((i+1)); if [ $((i % 10)) = 0 ]; then sleep 0.02; fi; done; echo DONE`)
	_ = r.m.Host().SendKeys(context.Background(), "term-a", "Enter")
	eventually(t, "producer to be under way", func() bool {
		s, _ := r.m.Host().CapturePane(context.Background(), "term-a")
		return strings.Contains(s, "L30")
	})
	tm := newTerm(t, 200, 50)
	out, done, cancel := start(t, r, tm, func(o *Options) { o.Observe = false })
	eventually(t, "producer to finish", func() bool { return strings.Contains(out.String(), "\nDONE\r\n") })
	cancel()
	wait(t, done)
	// Strip the typed command echo: only standalone numbered lines count.
	last := 0
	seen := map[int]int{}
	for _, ln := range strings.Split(out.String(), "\n") {
		ln = strings.TrimPrefix(strings.TrimSpace(ln), "$ ") // the first line follows the prompt
		var n int
		if len(ln) > 1 && ln[0] == 'L' && sscanInt(ln[1:], &n) {
			seen[n]++
			if n < last {
				t.Fatalf("line L%d arrived after L%d: out of order", n, last)
			}
			last = n
		}
	}
	for i := 1; i <= 150; i++ {
		if seen[i] != 1 {
			t.Fatalf("line L%d seen %d times, want exactly once (replay/live seam)", i, seen[i])
		}
	}
}

func sscanInt(s string, n *int) bool {
	v := 0
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
		v = v*10 + int(c-'0')
	}
	*n = v
	return true
}

// Reconnect: a dropped connection resumes; keystrokes typed around the drop run
// exactly once (counted by a side effect in a file, not by what is on screen —
// a resync legitimately redraws the screen).
func TestReconnectResumesWithoutDuplicatingInput(t *testing.T) {
	r := newRig(t)
	tm := newTerm(t, 100, 30)
	log := filepath.Join(t.TempDir(), "ran")
	var mu sync.Mutex
	var conns []*websocket.Conn
	_, done, cancel := start(t, r, tm, func(o *Options) {
		o.Backoff = func(int) time.Duration { return 10 * time.Millisecond }
		o.Dial = func(ctx context.Context, url string, h http.Header) (*websocket.Conn, error) {
			c, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: h})
			if err == nil {
				mu.Lock()
				conns = append(conns, c)
				mu.Unlock()
			}
			return c, err
		}
	})
	eventually(t, "raw", func() bool { return tm.isRaw(t) })
	ran := func() []string {
		b, _ := os.ReadFile(log)
		return strings.Fields(string(b))
	}
	tm.type_("echo first >> " + log + "\r")
	eventually(t, "first", func() bool { return len(ran()) == 1 })
	mu.Lock()
	_ = conns[0].CloseNow() // network drop
	mu.Unlock()
	tm.type_("echo second >> " + log + "\r")
	eventually(t, "second after the reconnect", func() bool { return len(ran()) == 2 })
	tm.type_("echo third >> " + log + "\r")
	eventually(t, "third", func() bool { return len(ran()) == 3 })
	if got := strings.Join(ran(), ","); got != "first,second,third" {
		t.Fatalf("commands ran as %q, want first,second,third exactly once each", got)
	}
	mu.Lock()
	n := len(conns)
	mu.Unlock()
	if n < 2 {
		t.Fatalf("expected a reconnect, saw %d connections", n)
	}
	cancel()
	wait(t, done)
}

// A tmux-hosted session is not attachable here: the client says so without
// rendering or sending anything.
func TestNotDirectIsReportedBeforeAnyOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		conn, err := websocket.Accept(w, req, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		_, _, _ = conn.Read(req.Context())
		_ = conn.Write(req.Context(), websocket.MessageText,
			[]byte(`{"type":"plan","plan":{"mode":"tail","head":1,"presentation":"tmux","session_id":"x","epoch":1}}`))
		_, _, _ = conn.Read(req.Context())
	}))
	defer srv.Close()
	tm := newTerm(t, 80, 24)
	out := &safeBuf{}
	res, err := Run(context.Background(), Options{URL: "ws" + strings.TrimPrefix(srv.URL, "http"), In: tm.tty, Out: out})
	if err != ErrNotDirect {
		t.Fatalf("err = %v (%+v), want ErrNotDirect", err, res)
	}
	if out.String() != "" {
		t.Fatalf("rendered %q before refusing", out.String())
	}
	if tm.isRaw(t) {
		t.Fatal("terminal left raw")
	}
}
