package repl

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// newShellOrSkip builds a Shell, skipping the test when no PTY/shell is
// available (CI containers without /dev/ptmx) — PTY behavior can't be unit
// tested hermetically.
func newShellOrSkip(t *testing.T, dir string, screen io.Writer) *Shell {
	t.Helper()
	sh, err := NewShell(dir, screen)
	if err != nil {
		t.Skipf("no usable shell/PTY in this environment: %v", err)
	}
	return sh
}

func TestShell_PassthroughRunsInRealShell(t *testing.T) {
	sh := newShellOrSkip(t, "", io.Discard)
	defer sh.Close()
	out, err := sh.Run(context.Background(), "echo $((1+1))")
	require.NoError(t, err)
	require.Contains(t, out.Captured, "2")
	require.Equal(t, 0, out.ExitCode)
}

func TestShell_PersistsCwd(t *testing.T) {
	sh := newShellOrSkip(t, "/", io.Discard)
	defer sh.Close()
	_, err := sh.Run(context.Background(), "cd /tmp")
	require.NoError(t, err)
	out, err := sh.Run(context.Background(), "pwd")
	require.NoError(t, err)
	require.Contains(t, out.Captured, "/tmp")
}

func TestShell_CaptureIsVerbatim(t *testing.T) {
	var screen bytes.Buffer
	sh := newShellOrSkip(t, "", &screen)
	defer sh.Close()
	out, err := sh.Run(context.Background(), "echo hello-verbatim")
	require.NoError(t, err)
	// Below the cap, the model's capture is exactly what the operator saw — no
	// paraphrase, no trimming.
	require.Equal(t, screen.String(), out.Captured)
	require.Contains(t, out.Captured, "hello-verbatim")
}

func TestShell_NonZeroExitSurfaced(t *testing.T) {
	sh := newShellOrSkip(t, "", io.Discard)
	defer sh.Close()
	// A failing command (subshell so it reports code 3 without killing the
	// persistent session); the host surfaces it but takes no further action.
	out, err := sh.Run(context.Background(), "(exit 3)")
	require.NoError(t, err)
	require.Equal(t, 3, out.ExitCode)
}

func TestShell_CaptureIsTailBounded(t *testing.T) {
	var screen bytes.Buffer
	sh := newShellOrSkip(t, "", &screen)
	defer sh.Close()
	out, err := sh.Run(context.Background(), "seq 1 1000")
	require.NoError(t, err)
	require.LessOrEqual(t, strings.Count(out.Captured, "\n"), maxCaptureLines)
	require.Contains(t, screen.String(), "1000", "the operator always sees the full stream")
}
