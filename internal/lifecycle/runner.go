package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// Runner executes an external command in an optional working directory and
// returns combined stdout. It is the single seam mocked in tests.
type Runner interface {
	Run(ctx context.Context, dir string, name string, args ...string) (string, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// HintingExecRunner wraps a Runner and enhances command-not-found errors
// with installation hints for tmux, claude, and gh.
type HintingExecRunner struct {
	Inner Runner
}

func (h HintingExecRunner) Run(ctx context.Context, dir, name string, args ...string) (string, error) {
	out, err := h.Inner.Run(ctx, dir, name, args...)
	if err != nil && isCommandNotFound(err) {
		hint := commandInstallHint(name)
		if hint != "" {
			return out, fmt.Errorf("%w\n\n%s", err, hint)
		}
	}
	return out, err
}

// isCommandNotFound checks if an error indicates a command was not found.
// Returns true for exec.ErrNotFound or errors containing "executable file not found".
func isCommandNotFound(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, exec.ErrNotFound) {
		return true
	}
	errStr := err.Error()
	return strings.Contains(errStr, "executable file not found") ||
		strings.Contains(errStr, "command not found") ||
		strings.Contains(errStr, "no such file or directory")
}

// commandInstallHint returns a platform-specific installation hint for a command.
func commandInstallHint(cmd string) string {
	// Extract base command name (strip path and fake suffixes for tests)
	base := cmd
	if idx := strings.LastIndex(cmd, "/"); idx >= 0 {
		base = cmd[idx+1:]
	}
	base = strings.TrimSuffix(base, "-fake")

	switch base {
	case "tmux":
		return "Install: brew install tmux (macOS) or apt install tmux (Linux)"
	case "gh":
		return "Install: brew install gh (macOS) or apt install gh (Linux)\nOr visit: https://cli.github.com"
	case "claude":
		return "Install Claude Code from https://claude.ai/download"
	default:
		return "Install " + base + " to continue"
	}
}

// --- test double ---

type FakeResp struct {
	Out string
	Err error
}

type FakeCall struct {
	Dir  string
	Argv []string
}

// FakeConfig is a test double for ConfigProvider.
type FakeConfig struct {
	PermissionMode string
	ModelDefault   string
	// PipelineHintOff disables the pipeline hint; the zero value leaves it on,
	// matching the production default.
	PipelineHintOff bool
	// CollabHintOff disables the collab hint; the zero value leaves it on,
	// matching the production default.
	CollabHintOff bool
}

func (f *FakeConfig) GetDefaultPermissionMode() string {
	if f.PermissionMode == "" {
		return "auto"
	}
	return f.PermissionMode
}

func (f *FakeConfig) GetModelDefault() string { return f.ModelDefault }

func (f *FakeConfig) GetPipelineHint() bool { return !f.PipelineHintOff }

func (f *FakeConfig) GetCollabHint() bool { return !f.CollabHintOff }

// FakeRunner matches on "name arg1 arg2 ..." joined by spaces.
type FakeRunner struct {
	Responses map[string]FakeResp
	Calls     []FakeCall
	// FailIf, when set and it returns a non-nil error for a call's argv, fails
	// that call. Use it to inject a failure on a command whose exact args aren't
	// known in advance (e.g. send-keys, which embeds a random claude session id).
	FailIf func(argv []string) error
}

func (f *FakeRunner) Run(_ context.Context, dir, name string, args ...string) (string, error) {
	argv := append([]string{name}, args...)
	f.Calls = append(f.Calls, FakeCall{Dir: dir, Argv: argv})
	if f.FailIf != nil {
		if err := f.FailIf(argv); err != nil {
			return "", err
		}
	}
	key := strings.Join(argv, " ")
	if r, ok := f.Responses[key]; ok {
		return r.Out, r.Err
	}
	return "", nil // unmatched calls succeed silently
}
