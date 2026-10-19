package ptyruntime

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// The scripted fake CLI is this test binary re-executed with fakeEnv set: a
// deterministic line-oriented REPL driven through a real PTY, standing in for
// an AI CLI (no hosted provider).
const fakeEnv = "PTYRUNTIME_FAKE_CLI"

func TestMain(m *testing.M) {
	switch os.Getenv(fakeEnv) {
	case "":
		os.Exit(m.Run())
	case "repl":
		fakeREPL()
	case "sleep":
		time.Sleep(time.Hour)
	}
	os.Exit(0)
}

// fakeArgv is the argv that starts the fake CLI.
func fakeArgv(t *testing.T) []string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return []string{exe}
}

func fakeEnviron(mode string) []string { return append(os.Environ(), fakeEnv+"="+mode) }

// fakeSecret is token-shaped at runtime only (no token literals in the repo).
func fakeSecret() string { return "sk-" + "ant-" + strings.Repeat("Q7", 16) }

func fakeREPL() {
	out := os.Stdout
	fmt.Fprint(out, "ready\n")
	in := bufio.NewReader(os.Stdin)
	for {
		line, err := in.ReadString('\n')
		if err != nil {
			return
		}
		cmd, arg, _ := strings.Cut(strings.TrimRight(line, "\r\n"), " ")
		switch cmd {
		case "say":
			fmt.Fprintf(out, "%s\n", arg)
		case "utf8":
			// Byte-at-a-time with pauses so multi-byte runes straddle reads.
			for _, b := range []byte("héllo→世界🙂\n") {
				out.Write([]byte{b})
				time.Sleep(2 * time.Millisecond)
			}
		case "half":
			out.Write([]byte("cut\xe4\xb8")) // incomplete rune, then silence
		case "controls":
			fmt.Fprint(out, "\x1b[?1049h\x1b]0;fake title\x07ALT\x1b[2J\x1b[?1049l\x1b[?25lmain\x1b[?25h\n")
		case "size":
			ws, err := unix.IoctlGetWinsize(0, unix.TIOCGWINSZ)
			if err == nil {
				fmt.Fprintf(out, "size=%dx%d\n", ws.Col, ws.Row)
			}
		case "secret":
			fmt.Fprintf(out, "your key is %s ok\n", fakeSecret())
		case "getpass":
			t, err := unix.IoctlGetTermios(0, ioctlGetTermios)
			if err != nil {
				return
			}
			saved := *t
			t.Lflag &^= unix.ECHO
			_ = unix.IoctlSetTermios(0, ioctlSetTermios, t)
			fmt.Fprint(out, "Password: ")
			pw, _ := in.ReadString('\n')
			_ = unix.IoctlSetTermios(0, ioctlSetTermios, &saved)
			fmt.Fprintf(out, "\ngot %d\n", len(strings.TrimRight(pw, "\r\n")))
		case "flood":
			n, _ := strconv.Atoi(arg)
			row := strings.Repeat("x", 99) + "\n"
			for i := 0; i < n; i++ {
				out.WriteString(row)
			}
			fmt.Fprint(out, "flood-done\n")
		case "child":
			c := exec.Command(os.Args[0])
			c.Env = append(os.Environ(), fakeEnv+"=sleep")
			_ = c.Start()
			fmt.Fprintf(out, "child %d\n", c.Process.Pid)
		case "stubborn":
			signal.Ignore(syscall.SIGTERM, syscall.SIGHUP)
			fmt.Fprint(out, "stubborn\n")
		case "exit":
			code, _ := strconv.Atoi(arg)
			os.Exit(code)
		case "kill":
			_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
	}
}
