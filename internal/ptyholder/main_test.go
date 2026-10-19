package ptyholder

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/ptyruntime"
)

// TestMain lets the test binary play the two processes the survival tests need
// without any external CLI: a holder (`__ptyholder <spec>`) and a stand-in
// daemon (`__daemon <root> <journals> <name>`) that is SIGKILLed mid-session.
func TestMain(m *testing.M) {
	if len(os.Args) >= 3 && os.Args[1] == "__ptyholder" {
		var ready *os.File
		if os.Getenv("WARDEN_PTYHOLDER_READY_FD") == "3" {
			ready = os.NewFile(3, "ready")
		}
		if err := Run(os.Args[2], ready); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if len(os.Args) >= 5 && os.Args[1] == "__daemon" {
		os.Exit(fakeDaemon(os.Args[2], os.Args[3], os.Args[4]))
	}
	os.Exit(m.Run())
}

// fakeDaemon hosts one shell through a holder, prints "PID <leader pid>" and
// "READY" once the shell has produced output, then blocks until killed.
func fakeDaemon(root, journals, name string) int {
	h := newHost(root, journals)
	cwd, _ := os.Getwd()
	if err := h.NewSession(context.Background(), name, cwd); err != nil {
		fmt.Println("ERR", err)
		return 1
	}
	s := h.Session(name)
	if err := h.SendKeys(context.Background(), name, "-l", "echo survive-marker-1"); err != nil {
		fmt.Println("ERR", err)
		return 1
	}
	_ = h.SendKeys(context.Background(), name, "Enter")
	fmt.Println("PID", s.PID())
	for end := time.Now().Add(15 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if strings.Contains(s.Screen(), "survive-marker-1") {
			fmt.Println("READY")
			select {}
		}
	}
	fmt.Println("ERR marker never appeared")
	return 1
}

func testLauncher(root string) *Launcher {
	exe, _ := os.Executable()
	return &Launcher{Root: root, Command: []string{exe, "__ptyholder"}, ExitedLinger: time.Minute}
}

func newHost(root, journals string) *ptyruntime.Host {
	return &ptyruntime.Host{JournalDir: journals, Shell: []string{"/bin/sh"}, Launcher: testLauncher(root),
		Options: ptyruntime.Options{KillGrace: 200 * time.Millisecond}}
}

func tmpDirs(t *testing.T) (root, journals string) {
	t.Helper()
	// A short path: the holder socket lives under root (sun_path is ~104 bytes).
	d, err := os.MkdirTemp("", "wph")
	if err != nil {
		t.Fatal(err)
	}
	root = filepath.Join(d, "h")
	t.Cleanup(func() { KillAll(root); os.RemoveAll(d) }) // no holder or shell outlives the test
	journals = filepath.Join(d, "j")
	return root, journals
}

// eventually polls cond until it holds or the deadline passes. It waits on
// observable state (not a fixed sleep) and fails loudly with why.
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
