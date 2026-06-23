//go:build integration

// Package integration holds warden's end-to-end suite: it builds the real
// `warden` binary, runs a real daemon subprocess against an isolated HOME +
// data dir, and drives it through the real CLI over HTTP. It is gated behind
// the `integration` build tag (and so excluded from the default `go test
// ./...`) because it spawns processes, binds a port, and — for the spawn path —
// optionally shells out to tmux/claude. Run it with `make test-integration`.
package integration

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// wardenBin is the path to the binary built once in TestMain.
var wardenBin string

// TestMain builds the warden binary a single time for the whole suite, so each
// test pays only for daemon startup, not a full compile.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "warden-itest-bin-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "mktemp:", err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)

	wardenBin = filepath.Join(dir, "warden")
	// Build from the module root (two levels up from test/integration).
	build := exec.Command("go", "build", "-buildvcs=false", "-o", wardenBin, "./cmd/warden")
	build.Dir = repoRoot()
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "build warden:", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// repoRoot returns the module root by walking up from the test directory until
// it finds go.mod.
func repoRoot() string {
	dir, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			panic("go.mod not found above " + dir)
		}
		dir = parent
	}
}

// harness is one isolated daemon under test: its own HOME (and thus data dir),
// its own port, and the subprocess running it.
type harness struct {
	t    *testing.T
	home string
	addr string
	cmd  *exec.Cmd
}

// startDaemon brings up a fresh daemon on a free port with an isolated HOME and
// waits for it to report healthy. Teardown is registered via t.Cleanup.
func startDaemon(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		t:    t,
		home: t.TempDir(),
		addr: fmt.Sprintf("127.0.0.1:%d", freePort(t)),
	}
	h.launch()
	t.Cleanup(h.stop)
	return h
}

// launch starts (or restarts) the daemon subprocess and blocks until /healthz
// returns 200 or the deadline passes.
func (h *harness) launch() {
	h.t.Helper()
	cmd := exec.Command(wardenBin, "daemon", "--addr", h.addr)
	cmd.Env = append(os.Environ(), "HOME="+h.home)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		h.t.Fatalf("start daemon: %v", err)
	}
	h.cmd = cmd

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if h.healthy() {
			return
		}
		// Surface an early crash instead of waiting the full timeout.
		if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
			h.t.Fatalf("daemon exited during startup:\n%s", out.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	h.stop()
	h.t.Fatalf("daemon did not become healthy on %s within 10s:\n%s", h.addr, out.String())
}

// healthy reports whether GET /healthz returns 200.
func (h *harness) healthy() bool {
	resp, err := http.Get("http://" + h.addr + "/healthz")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// stop terminates the daemon subprocess if it is still running.
func (h *harness) stop() {
	if h.cmd == nil || h.cmd.Process == nil {
		return
	}
	_ = h.cmd.Process.Signal(os.Interrupt)
	done := make(chan struct{})
	go func() { _ = h.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = h.cmd.Process.Kill()
		<-done
	}
	h.cmd = nil
}

// restart stops and re-launches the daemon against the same HOME/addr, so a
// test can assert that on-disk state survives a daemon lifecycle.
func (h *harness) restart() {
	h.t.Helper()
	h.stop()
	h.launch()
}

// wd runs a warden CLI subcommand against this harness's daemon and HOME,
// returning combined output and any error.
func (h *harness) wd(args ...string) (string, error) {
	h.t.Helper()
	full := append([]string{"--addr", h.addr}, args...)
	cmd := exec.Command(wardenBin, full...)
	cmd.Env = append(os.Environ(), "HOME="+h.home)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// mustWd runs a CLI subcommand and fails the test if it returns an error.
func (h *harness) mustWd(args ...string) string {
	h.t.Helper()
	out, err := h.wd(args...)
	if err != nil {
		h.t.Fatalf("warden %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// dataDir is the daemon's on-disk state directory (HOME/.warden).
func (h *harness) dataDir() string { return filepath.Join(h.home, ".warden") }

// freePort asks the OS for an unused TCP port and returns it. There is an
// inherent race between closing the listener and the daemon binding, but it is
// vanishingly small on a loopback test host.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// hasBinary reports whether name resolves on PATH (used to gate the spawn
// suite on claude/tmux availability).
func hasBinary(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}
