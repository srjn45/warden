package notify

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOSANotifierBuildsScript(t *testing.T) {
	var gotName string
	var gotArgs []string
	n := osaNotifier{run: func(name string, args ...string) error { gotName = name; gotArgs = args; return nil }}
	n.Notify("warden — needs input", `agent-a1b2: review auth`)
	require.Equal(t, "osascript", gotName)
	require.Len(t, gotArgs, 2)
	require.Equal(t, "-e", gotArgs[0])
	require.Contains(t, gotArgs[1], "display notification")
	require.Contains(t, gotArgs[1], "with title")
	require.Contains(t, gotArgs[1], "needs input")
	require.Contains(t, gotArgs[1], "agent-a1b2")
}

func TestNewSelectsByPlatformAndEnabled(t *testing.T) {
	require.IsType(t, logNotifier{}, New(false), "disabled → log notifier")
	// New() on darwin always picks osaNotifier; on linux it depends on PATH.
	// The newWith tests cover the branching exhaustively — this just checks the
	// public constructor forwards enabled=false correctly on any platform.
}

func TestNotifySendNotifierBuildsArgs(t *testing.T) {
	var gotName string
	var gotArgs []string
	n := notifySendNotifier{run: func(name string, args ...string) error {
		gotName = name
		gotArgs = args
		return nil
	}}
	n.Notify("warden — needs input", "agent-a1b2: review auth")
	require.Equal(t, "notify-send", gotName)
	require.Equal(t, []string{"warden — needs input", "agent-a1b2: review auth"}, gotArgs)
}

func TestNotifySendNotifierLogsError(t *testing.T) {
	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(os.Stderr)
	n := notifySendNotifier{run: func(name string, args ...string) error {
		return fmt.Errorf("mock failure")
	}}
	n.Notify("title", "body") // must not panic or propagate error
	require.Contains(t, logBuf.String(), "notify-send")
}

func TestNewWithLinux(t *testing.T) {
	// notify-send found → notifySendNotifier
	lookFound := func(string) (string, error) { return "/usr/bin/notify-send", nil }
	require.IsType(t, notifySendNotifier{}, newWith(true, execRun, "linux", lookFound))

	// notify-send not found → logNotifier
	lookMissing := func(string) (string, error) { return "", fmt.Errorf("not found") }
	require.IsType(t, logNotifier{}, newWith(true, execRun, "linux", lookMissing))

	// disabled → logNotifier regardless
	require.IsType(t, logNotifier{}, newWith(false, execRun, "linux", lookFound))
}

func TestNewWithDarwin(t *testing.T) {
	look := func(string) (string, error) { return "", nil } // unused on darwin
	require.IsType(t, osaNotifier{}, newWith(true, execRun, "darwin", look))
	require.IsType(t, logNotifier{}, newWith(false, execRun, "darwin", look))
}

// keep runtime imported so earlier tests still compile on non-darwin
var _ = runtime.GOOS
