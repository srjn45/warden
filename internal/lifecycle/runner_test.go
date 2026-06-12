package lifecycle

import (
	"context"
	"errors"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFakeRunnerRecordsCalls(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"git status --porcelain": {Out: ""},
	}}
	out, err := fr.Run(context.Background(), "", "git", "status", "--porcelain")
	require.NoError(t, err)
	require.Equal(t, "", out)
	require.Len(t, fr.Calls, 1)
	require.Equal(t, []string{"git", "status", "--porcelain"}, fr.Calls[0].Argv)
}

func TestFakeRunnerKeyMatch(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"tmux has-session -t A-1": {Err: errStub("no session")},
	}}
	_, err := fr.Run(context.Background(), "", "tmux", "has-session", "-t", "A-1")
	require.Error(t, err)
}

func TestHintingExecRunnerWrapsTmuxNotFound(t *testing.T) {
	stub := &stubRunner{err: exec.ErrNotFound}
	h := HintingExecRunner{Inner: stub}
	_, err := h.Run(context.Background(), "", "tmux", "new-session")
	require.Error(t, err)
	// Should wrap original error with %w
	require.True(t, errors.Is(err, exec.ErrNotFound))
	require.Contains(t, err.Error(), "brew install tmux")
}

func TestHintingExecRunnerWrapsClaudeNotFound(t *testing.T) {
	stub := &stubRunner{err: errors.New("executable file not found in $PATH")}
	h := HintingExecRunner{Inner: stub}
	_, err := h.Run(context.Background(), "", "claude", "--version")
	require.Error(t, err)
	// Should wrap original error with %w
	require.Contains(t, err.Error(), "executable file not found")
	require.Contains(t, err.Error(), "https://claude.ai/download")
}

func TestHintingExecRunnerWrapsGhNotFound(t *testing.T) {
	stub := &stubRunner{err: exec.ErrNotFound}
	h := HintingExecRunner{Inner: stub}
	_, err := h.Run(context.Background(), "", "gh", "pr", "list")
	require.Error(t, err)
	// Should wrap original error with %w
	require.True(t, errors.Is(err, exec.ErrNotFound))
	require.Contains(t, err.Error(), "https://cli.github.com")
}

func TestHintingExecRunnerPassesOtherErrors(t *testing.T) {
	stub := &stubRunner{err: errors.New("permission denied")}
	h := HintingExecRunner{Inner: stub}
	_, err := h.Run(context.Background(), "", "tmux", "new-session")
	require.Error(t, err)
	require.Equal(t, "permission denied", err.Error())
}

func TestHintingExecRunnerPassesSuccess(t *testing.T) {
	stub := &stubRunner{out: "output"}
	h := HintingExecRunner{Inner: stub}
	out, err := h.Run(context.Background(), "", "git", "status")
	require.NoError(t, err)
	require.Equal(t, "output", out)
}

func TestIsCommandNotFoundDetectsExecErrNotFound(t *testing.T) {
	require.True(t, isCommandNotFound(exec.ErrNotFound))
}

func TestIsCommandNotFoundDetectsMessagePattern(t *testing.T) {
	require.True(t, isCommandNotFound(errors.New("executable file not found in $PATH")))
	require.True(t, isCommandNotFound(errors.New("command not found")))
	require.True(t, isCommandNotFound(errors.New("no such file or directory")))
}

func TestIsCommandNotFoundReturnsFalseForNil(t *testing.T) {
	require.False(t, isCommandNotFound(nil))
}

func TestIsCommandNotFoundReturnsFalseForOtherErrors(t *testing.T) {
	require.False(t, isCommandNotFound(errors.New("permission denied")))
}

func TestCommandInstallHintTmux(t *testing.T) {
	hint := commandInstallHint("tmux")
	require.Contains(t, hint, "brew install tmux")
	require.Contains(t, hint, "apt install tmux")
}

func TestCommandInstallHintClaude(t *testing.T) {
	hint := commandInstallHint("claude")
	require.Contains(t, hint, "https://claude.ai/download")
}

func TestCommandInstallHintGh(t *testing.T) {
	hint := commandInstallHint("gh")
	require.Contains(t, hint, "https://cli.github.com")
}

func TestCommandInstallHintUnknownCommand(t *testing.T) {
	hint := commandInstallHint("unknown")
	require.Contains(t, hint, "Install unknown")
}

func TestCommandInstallHintStripsFakeSuffix(t *testing.T) {
	// Test that "-fake" suffix is stripped before switch statement
	hint := commandInstallHint("tmux-fake")
	require.Contains(t, hint, "brew install tmux")
	require.Contains(t, hint, "apt install tmux")

	hint = commandInstallHint("/path/to/claude-fake")
	require.Contains(t, hint, "https://claude.ai/download")
}

type errStub string

func (e errStub) Error() string { return string(e) }

// stubRunner is a test double that returns fixed output/error
type stubRunner struct {
	out string
	err error
}

func (s *stubRunner) Run(_ context.Context, _ string, _ string, _ ...string) (string, error) {
	return s.out, s.err
}
