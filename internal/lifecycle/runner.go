package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Runner executes an external command in an optional working directory and
// returns combined stdout. It is the single seam mocked in tests.
type Runner interface {
	Run(ctx context.Context, dir string, name string, args ...string) (string, error)
}

// ExecRunner runs subprocesses. When the command is tmux, $TMUX is scrubbed from
// the child environment so spawn/liveness/pane capture always target the default
// tmux server (or TMUX_TMPDIR when set), not an interactive outer session the
// daemon inherited — without this, a restart that changes launch context (e.g.
// systemd vs a shell inside tmux) makes has-session look at the wrong server and
// marks live agents orphaned.
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	if isTmuxCommand(name) {
		cmd.Env = scrubTMUXFromEnviron(os.Environ())
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// isTmuxCommand reports whether name invokes the tmux client (path-safe).
func isTmuxCommand(name string) bool {
	base := name
	if idx := strings.LastIndex(name, "/"); idx >= 0 {
		base = name[idx+1:]
	}
	return base == "tmux"
}

// scrubTMUXFromEnviron returns a copy of env with TMUX removed so tmux resolves
// the server from TMUX_TMPDIR / the default socket instead of an inherited session.
func scrubTMUXFromEnviron(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.HasPrefix(kv, "TMUX=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// HintingExecRunner wraps a Runner and enhances command-not-found errors
// with installation hints for tmux, git, claude, gh, and ollama.
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
	case "git":
		return "Install: brew install git (macOS) or apt install git (Linux)"
	case "gh":
		return "Install: brew install gh (macOS) or apt install gh (Linux)\nOr visit: https://cli.github.com"
	case "claude":
		return "Install Claude Code: curl -fsSL https://claude.ai/install.sh | bash (macOS/Linux)\nOr visit: https://claude.ai/download"
	case "ollama":
		return "Install: brew install ollama (macOS) or curl -fsSL https://ollama.com/install.sh | sh (Linux)"
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
	// IsolationGuardOff disables the PreToolUse isolation guard; the zero value
	// leaves it on, matching the production default.
	IsolationGuardOff bool
	// GitConventionsOff disables the git-conventions system-prompt hint; the zero
	// value leaves it on, matching the production default.
	GitConventionsOff bool
	// GitRedirectOff disables the PreToolUse git-redirect hook; the zero value
	// leaves it on, matching the production default.
	GitRedirectOff bool
	// CheckRedirectOff disables the PreToolUse check-redirect hook; the zero value
	// leaves it on, matching the production default.
	CheckRedirectOff bool
	// RootGuardOff disables the PreToolUse root-guard hook; the zero value leaves
	// it on, matching the production default.
	RootGuardOff bool
	// MemoryInjectOff disables .warden/memory.md projection; the zero value leaves
	// it on, matching the production default.
	MemoryInjectOff bool
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

// GetMemoryInject mirrors production's default-on: the zero value projects
// .warden/memory.md; set MemoryInjectOff to exercise the disabled path.
func (f *FakeConfig) GetMemoryInject() bool { return !f.MemoryInjectOff }

func (f *FakeConfig) GetIsolationGuard() bool { return !f.IsolationGuardOff }

func (f *FakeConfig) GetGitConventions() bool { return !f.GitConventionsOff }

func (f *FakeConfig) GetGitRedirect() bool { return !f.GitRedirectOff }

func (f *FakeConfig) GetCheckRedirect() bool { return !f.CheckRedirectOff }

func (f *FakeConfig) GetRootGuard() bool { return !f.RootGuardOff }

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
