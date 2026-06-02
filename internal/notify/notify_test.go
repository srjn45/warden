package notify

import (
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOSANotifierBuildsScript(t *testing.T) {
	var gotName string
	var gotArgs []string
	n := osaNotifier{run: func(name string, args ...string) error { gotName = name; gotArgs = args; return nil }}
	n.Notify("agentctl — needs input", `agent-a1b2: review auth`)
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
	if runtime.GOOS == "darwin" {
		require.IsType(t, osaNotifier{}, New(true), "darwin+enabled → osa notifier")
	} else {
		require.IsType(t, logNotifier{}, New(true), "non-darwin → log notifier")
	}
}
