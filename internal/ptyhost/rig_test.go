package ptyhost

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/srjn45/warden/internal/ptyevent"
	"github.com/srjn45/warden/internal/ptyruntime"
)

const waitFor = 10 * time.Second

// fakeEnv re-executes this test binary as a scripted, deterministic fake CLI
// on a real PTY (no hosted provider): it prints "ready", then answers one
// command per line.
const fakeEnv = "PTYHOST_FAKE_CLI"

func TestMain(m *testing.M) {
	if len(os.Args) == 4 && os.Args[1] == "__tmux_scope" {
		r := tmuxRunner{sock: os.Args[2], token: os.Args[3]}
		_, _ = io.Copy(io.Discard, os.Stdin) // lease closes on test exit, including SIGKILL
		if err := r.cleanup(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if os.Getenv(fakeEnv) == "repl" {
		fakeREPL()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeSecret is token-shaped at runtime only (no token literals in the repo).
func fakeSecret() string { return "sk-" + "ant-" + strings.Repeat("Z3", 16) }

func fakeREPL() {
	out := os.Stdout
	fmt.Fprint(out, "ready\r\n")
	in := bufio.NewReader(os.Stdin)
	for {
		line, err := in.ReadString('\n')
		if err != nil {
			return
		}
		cmd, arg, _ := strings.Cut(strings.TrimRight(line, "\r\n"), " ")
		switch cmd {
		case "echo":
			fmt.Fprintf(out, "said:%s\r\n", arg)
		case "secret":
			fmt.Fprintf(out, "token=%s\r\n", fakeSecret())
		case "size":
			ws, err := unix.IoctlGetWinsize(0, unix.TIOCGWINSZ)
			if err == nil {
				fmt.Fprintf(out, "size:%dx%d\r\n", ws.Col, ws.Row)
			}
		case "exit":
			return
		}
	}
}

func fakeShell(t *testing.T) []string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return []string{exe}
}

// tmuxRunner runs tmux against an isolated server socket.
type tmuxRunner struct{ sock, token string }

func (r tmuxRunner) Run(ctx context.Context, dir, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if name == "tmux" {
		if len(args) > 0 && args[0] == "new-session" && r.token != "" {
			for i := 1; i+1 < len(args); i++ {
				if args[i] == "-s" {
					session := args[i+1]
					args = append(args, ";", "set-option", "-t", session, "@warden_test_owner", r.token)
					break
				}
			}
		}
		args = append([]string{"-L", r.sock, "-f", "/dev/null"}, args...)
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.WaitDelay = 200 * time.Millisecond
	env := make([]string, 0, len(os.Environ())+1)
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "TMUX=") && !strings.HasPrefix(e, "SHELL=") {
			env = append(env, e)
		}
	}
	cmd.Env = append(env, "SHELL=/bin/sh")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func newTmux(t *testing.T) tmuxRunner {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	r := tmuxRunner{sock: fmt.Sprintf("wd-ptyhost-%d-%d", os.Getpid(), time.Now().UnixNano()), token: fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())}
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	exe, _ := os.Executable()
	guard := exec.Command(exe, "__tmux_scope", r.sock, r.token)
	guard.Stdin, guard.Stderr = rd, os.Stderr
	if err := guard.Start(); err != nil {
		rd.Close()
		wr.Close()
		t.Fatal(err)
	}
	rd.Close()
	done := make(chan error, 1)
	go func() { done <- guard.Wait() }()
	t.Cleanup(func() {
		wr.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("tmux scope cleanup: %v", err)
			}
		case <-time.After(5 * time.Second):
			_ = guard.Process.Kill()
			t.Error("tmux scope cleanup timeout")
		}
	})
	return r
}

// cleanup targets only session IDs carrying this fixture's ownership token.
// The private server exits naturally when its last session is removed, so a
// server that is gone (or exits mid-command) counts as clean once a re-list
// confirms no owned session survives. Only r.sock is ever addressed.
func (r tmuxRunner) cleanup() error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var lastErr error
	for attempt := 0; attempt < 10; attempt++ {
		if attempt > 0 {
			time.Sleep(50 * time.Millisecond)
		}
		out, err := r.Run(ctx, "", "tmux", "list-sessions", "-F", "#{session_id} #{@warden_test_owner}")
		if err != nil {
			if tmuxServerGone(out) {
				return nil
			}
			lastErr = fmt.Errorf("list test tmux sessions: %w: %s", err, out)
			if strings.Contains(out, "server exited unexpectedly") {
				continue // server shutting down; re-list to confirm it is gone
			}
			return lastErr
		}
		var owned []string
		for _, line := range strings.Split(out, "\n") {
			if fields := strings.Fields(line); len(fields) == 2 && fields[1] == r.token {
				owned = append(owned, fields[0])
			}
		}
		if len(owned) == 0 {
			return nil
		}
		lastErr = nil
		for _, id := range owned {
			if out, err := r.Run(ctx, "", "tmux", "kill-session", "-t", id); err != nil && !tmuxServerGone(out) &&
				!strings.Contains(out, "server exited unexpectedly") && !strings.Contains(out, "can't find session") {
				lastErr = fmt.Errorf("cleanup test tmux session %s: %w: %s", id, err, out)
			}
		}
		// loop re-lists to verify the kills took effect
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("owned test tmux sessions survived cleanup on %s", r.sock)
	}
	return lastErr
}

func tmuxServerGone(out string) bool {
	return strings.Contains(out, "no server running") || strings.Contains(out, "No such file") ||
		strings.Contains(out, "error connecting")
}

func newManager(t *testing.T, run tmuxRunner, dir string, mode Mode) *Manager {
	t.Helper()
	m := New(Config{Mode: mode, Dir: dir, Run: run, Shell: fakeShell(t), ReconcileEvery: 50 * time.Millisecond,
		Options: ptyruntime.Options{IdleFlush: 20 * time.Millisecond, KillGrace: 500 * time.Millisecond, DrainGrace: 300 * time.Millisecond}})
	return m
}

// noLeaks fails the test if a runtime/host goroutine outlives it.
func noLeaks(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		deadline := time.Now().Add(3 * time.Second)
		for {
			buf := make([]byte, 1<<20)
			stacks := string(buf[:runtime.Stack(buf, true)])
			var leaked []string
			for _, g := range strings.Split(stacks, "\n\n") {
				if strings.Contains(g, "internal/ptyruntime.") || strings.Contains(g, "internal/ptyhost.(*Manager)") {
					leaked = append(leaked, g)
				}
			}
			if len(leaked) == 0 {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%d leaked goroutines:\n%s", len(leaked), strings.Join(leaked, "\n\n"))
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
}

// journal reads a session journal back from disk.
func journal(t *testing.T, dir, name string) []ptyevent.Event {
	t.Helper()
	j, err := ptyruntime.OpenJournal(dir, name, ptyruntime.Retention{})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	var evs []ptyevent.Event
	if err := j.Read(1, func(ev ptyevent.Event) error { evs = append(evs, ev); return nil }); err != nil {
		t.Fatal(err)
	}
	return evs
}

// liveOutput reads a live session's output through its own journal (opening
// a second Journal on a live directory could truncate a record mid-write).
func liveOutput(t *testing.T, m *Manager, name string) string {
	t.Helper()
	return outputOf(liveEvents(t, m, name))
}

func liveEvents(t *testing.T, m *Manager, name string) []ptyevent.Event {
	t.Helper()
	tg, ok := m.Lookup(name)
	if !ok {
		return nil
	}
	var evs []ptyevent.Event
	_ = tg.Stream.Journal().Read(1, func(ev ptyevent.Event) error { evs = append(evs, ev); return nil })
	return evs
}

func outputOf(evs []ptyevent.Event) string {
	var b strings.Builder
	for _, ev := range evs {
		if ev.Kind == ptyevent.KindOutput {
			b.Write(ev.Output.Data)
		}
	}
	return b.String()
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitFor)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func lastLifecycle(evs []ptyevent.Event) *ptyevent.Lifecycle {
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Kind == ptyevent.KindLifecycle {
			return evs[i].Lifecycle
		}
	}
	return nil
}

func validStream(t *testing.T, evs []ptyevent.Event) {
	t.Helper()
	if vs := ptyevent.ValidateStream(evs); len(vs) > 0 {
		t.Fatalf("stream violations: %v", vs)
	}
}

func shadowDir(dir, name string) string { return filepath.Join(dir, "shadow", name) }
func directDir(dir, name string) string { return filepath.Join(dir, "direct", name) }
