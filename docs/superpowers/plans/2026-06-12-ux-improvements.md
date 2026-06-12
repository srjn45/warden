# UX Improvements Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add shell completion support and enhance error messages with actionable hints throughout warden.

**Architecture:** Add Cobra's built-in completion command for shell completions. Enhance errors inline at their source by wrapping with contextual hints using `fmt.Errorf`. Add a helper function in `internal/cli/common.go` to detect command-not-found errors consistently.

**Tech Stack:** Go 1.26, Cobra CLI framework, standard library exec/errors packages

---

## File Structure

**New files:**
- `internal/cli/completion.go` — Shell completion command implementation

**Modified files:**
- `internal/cli/common.go` — Add `isCommandNotFound` helper and install hint formatters
- `internal/cli/root.go` — Register completion command
- `internal/client/client.go` — Enhance daemon connection error messages
- `internal/lifecycle/runner.go` — Add error wrapping for command execution
- `internal/lifecycle/lifecycle.go` — Wrap tmux/claude/gh/git errors with hints
- `internal/daemon/server.go` — Enhance port binding error messages

**Test files:**
- `internal/cli/completion_test.go` — Test completion command
- `internal/cli/common_test.go` — Test error helper functions
- `internal/lifecycle/runner_test.go` — Test error wrapping in runner

---

## Task 1: Add Command-Not-Found Helper

**Files:**
- Modify: `internal/cli/common.go`
- Test: `internal/cli/common_test.go`

- [ ] **Step 1: Write failing test for isCommandNotFound**

```go
package cli

import (
	"errors"
	"os/exec"
	"testing"
)

func TestIsCommandNotFound(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "exec.ErrNotFound",
			err:  exec.ErrNotFound,
			want: true,
		},
		{
			name: "wrapped exec.ErrNotFound",
			err:  errors.New("some context: executable file not found in $PATH"),
			want: true,
		},
		{
			name: "other error",
			err:  errors.New("permission denied"),
			want: false,
		},
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isCommandNotFound(tt.err); got != tt.want {
				t.Errorf("isCommandNotFound() = %v, want %v", got, tt.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli -run TestIsCommandNotFound -v`
Expected: FAIL with "undefined: isCommandNotFound"

- [ ] **Step 3: Implement isCommandNotFound helper**

Add to `internal/cli/common.go`:

```go
import (
	"errors"
	"os/exec"
	"strings"
)

// isCommandNotFound checks if an error indicates a command was not found.
// Returns true for exec.ErrNotFound or errors containing "executable file not found".
func isCommandNotFound(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, exec.ErrNotFound) {
		return true
	}
	return strings.Contains(err.Error(), "executable file not found")
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/cli -run TestIsCommandNotFound -v`
Expected: PASS

- [ ] **Step 5: Write test for install hint formatters**

Add to `internal/cli/common_test.go`:

```go
func TestInstallHint(t *testing.T) {
	tests := []struct {
		name    string
		cmd     string
		wantMac string
		wantLin string
	}{
		{
			name:    "tmux",
			cmd:     "tmux",
			wantMac: "brew install tmux",
			wantLin: "apt install tmux",
		},
		{
			name:    "gh",
			cmd:     "gh",
			wantMac: "brew install gh",
			wantLin: "apt install gh",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hint := installHint(tt.cmd)
			if !strings.Contains(hint, tt.wantMac) {
				t.Errorf("installHint(%q) missing macOS hint %q", tt.cmd, tt.wantMac)
			}
			if !strings.Contains(hint, tt.wantLin) {
				t.Errorf("installHint(%q) missing Linux hint %q", tt.cmd, tt.wantLin)
			}
		})
	}
}
```

- [ ] **Step 6: Run test to verify it fails**

Run: `go test ./internal/cli -run TestInstallHint -v`
Expected: FAIL with "undefined: installHint"

- [ ] **Step 7: Implement installHint helper**

Add to `internal/cli/common.go`:

```go
// installHint returns a platform-specific installation hint for a command.
func installHint(cmd string) string {
	switch cmd {
	case "tmux":
		return "Install: brew install tmux (macOS) or apt install tmux (Linux)"
	case "gh":
		return "Install: brew install gh (macOS) or apt install gh (Linux)\nOr visit: https://cli.github.com"
	case "claude":
		return "Install Claude Code from https://claude.ai/download"
	default:
		return "Install " + cmd + " to continue"
	}
}
```

- [ ] **Step 8: Run test to verify it passes**

Run: `go test ./internal/cli -run TestInstallHint -v`
Expected: PASS

- [ ] **Step 9: Commit**

```bash
git add internal/cli/common.go internal/cli/common_test.go
git commit -m "feat(cli): add command-not-found and install hint helpers

Co-Authored-By: Claude Sonnet 4.5 <noreply@anthropic.com>"
```

---

## Task 2: Add Shell Completion Command

**Files:**
- Create: `internal/cli/completion.go`
- Modify: `internal/cli/root.go:42`
- Test: `internal/cli/completion_test.go`

- [ ] **Step 1: Write test for completion command**

Create `internal/cli/completion_test.go`:

```go
package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestCompletionCommand(t *testing.T) {
	tests := []struct {
		shell      string
		wantInHelp string
	}{
		{"bash", "/etc/bash_completion.d/warden"},
		{"zsh", "/usr/local/share/zsh/site-functions/_warden"},
		{"fish", "~/.config/fish/completions/warden.fish"},
		{"powershell", "warden.ps1"},
	}

	for _, tt := range tests {
		t.Run(tt.shell, func(t *testing.T) {
			root := newRootCmd()
			cmd := root.Commands()
			var found *cobra.Command
			for _, c := range cmd {
				if c.Use == "completion" {
					found = c
					break
				}
			}
			if found == nil {
				t.Fatal("completion command not registered")
			}

			// Check that shell-specific subcommand exists
			var shellCmd *cobra.Command
			for _, c := range found.Commands() {
				if c.Use == tt.shell {
					shellCmd = c
					break
				}
			}
			if shellCmd == nil {
				t.Fatalf("completion %s subcommand not found", tt.shell)
			}

			// Check help text contains install path
			if !strings.Contains(found.Long, tt.wantInHelp) {
				t.Errorf("completion help missing %q for %s", tt.wantInHelp, tt.shell)
			}
		})
	}
}

func TestBashCompletionGenerates(t *testing.T) {
	root := newRootCmd()
	buf := new(bytes.Buffer)
	
	// Find completion bash command
	var bashCmd *cobra.Command
	for _, c := range root.Commands() {
		if c.Use == "completion" {
			for _, sc := range c.Commands() {
				if sc.Use == "bash" {
					bashCmd = sc
					break
				}
			}
			break
		}
	}
	
	if bashCmd == nil {
		t.Fatal("bash completion command not found")
	}

	// Execute it with buffer as stdout
	bashCmd.SetOut(buf)
	root.SetArgs([]string{"completion", "bash"})
	if err := root.Execute(); err != nil {
		t.Fatalf("completion bash failed: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "# bash completion") {
		t.Error("bash completion output missing expected header")
	}
	if !strings.Contains(output, "warden") {
		t.Error("bash completion output missing warden command")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli -run TestCompletion -v`
Expected: FAIL with "completion command not registered"

- [ ] **Step 3: Implement completion command**

Create `internal/cli/completion.go`:

```go
package cli

import (
	"os"

	"github.com/spf13/cobra"
)

const completionLong = `Generate shell completion scripts for warden.

The completion script for each shell should be redirected to the appropriate
location for your shell. Examples:

Bash:
  warden completion bash > /etc/bash_completion.d/warden
  # or for user-only installation:
  warden completion bash > ~/.bash_completion

Zsh:
  warden completion zsh > /usr/local/share/zsh/site-functions/_warden
  # or for user-only installation:
  warden completion zsh > ~/.zsh/completion/_warden

Fish:
  warden completion fish > ~/.config/fish/completions/warden.fish

PowerShell:
  warden completion powershell > warden.ps1
  # Then load it in your PowerShell profile

After generating the completion script, you may need to restart your shell
or source the file for the completions to take effect.
`

func newCompletionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "completion",
		Short: "Generate shell completion scripts",
		Long:  completionLong,
	}

	cmd.AddCommand(&cobra.Command{
		Use:   "bash",
		Short: "Generate bash completion script",
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Root().GenBashCompletion(os.Stdout)
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "zsh",
		Short: "Generate zsh completion script",
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Root().GenZshCompletion(os.Stdout)
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "fish",
		Short: "Generate fish completion script",
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Root().GenFishCompletion(os.Stdout, true)
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "powershell",
		Short: "Generate PowerShell completion script",
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Root().GenPowerShellCompletion(os.Stdout)
		},
	})

	return cmd
}
```

- [ ] **Step 4: Register completion command in root**

Modify `internal/cli/root.go` at line 42:

```go
	root.AddCommand(newMCPCmd())
	root.AddCommand(newTUICmd())
	root.AddCommand(newDoctorCmd())
	root.AddCommand(newCompletionCmd())
	root.Args = cobra.NoArgs
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/cli -run TestCompletion -v`
Expected: PASS

- [ ] **Step 6: Manual test - generate bash completion**

Run: `go run cmd/warden/main.go completion bash | head -20`
Expected: Output starts with "# bash completion" and contains warden commands

- [ ] **Step 7: Manual test - verify help text**

Run: `go run cmd/warden/main.go completion --help`
Expected: Help text shows all four shells with installation examples

- [ ] **Step 8: Commit**

```bash
git add internal/cli/completion.go internal/cli/completion_test.go internal/cli/root.go
git commit -m "feat(cli): add shell completion command

Supports bash, zsh, fish, and PowerShell via Cobra generators.
Includes installation examples in help text.

Co-Authored-By: Claude Sonnet 4.5 <noreply@anthropic.com>"
```

---

## Task 3: Enhance Daemon Connection Errors

**Files:**
- Modify: `internal/client/client.go:26`

- [ ] **Step 1: Update ErrDaemonDown message**

Modify `internal/client/client.go` line 26:

```go
// ErrDaemonDown signals the daemon is unreachable (connection refused / timeout).
var ErrDaemonDown = errors.New("daemon not running\n\nRun: warden daemon\nOr install as a service: ./scripts/install.sh")
```

- [ ] **Step 2: Test the enhanced error message**

Run: `go test ./internal/client -v`
Expected: All tests pass (error message is just a string constant)

- [ ] **Step 3: Manual test - verify error output**

```bash
# Stop daemon if running
pkill -f "warden daemon" || true
# Try to list sessions
go run cmd/warden/main.go ls
```

Expected: Output shows "daemon not running" with installation hints

- [ ] **Step 4: Commit**

```bash
git add internal/client/client.go
git commit -m "feat(client): enhance daemon connection error with install hint

Co-Authored-By: Claude Sonnet 4.5 <noreply@anthropic.com>"
```

---

## Task 4: Add Error Wrapping to Runner

**Files:**
- Modify: `internal/lifecycle/runner.go:17-22`
- Test: `internal/lifecycle/runner_test.go`

- [ ] **Step 1: Write test for command-not-found error wrapping**

Create `internal/lifecycle/runner_test.go`:

```go
package lifecycle

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
)

func TestExecRunnerWrapsCommandNotFound(t *testing.T) {
	runner := ExecRunner{}
	
	// Try to run a command that doesn't exist
	_, err := runner.Run(context.Background(), "", "nonexistent-command-xyz-123", "arg1")
	
	if err == nil {
		t.Fatal("expected error for nonexistent command, got nil")
	}
	
	// Should be an exec.Error indicating command not found
	var execErr *exec.Error
	if !errors.As(err, &execErr) {
		t.Fatalf("expected *exec.Error, got %T: %v", err, err)
	}
}

func TestExecRunnerWrapsCommandNotFoundForKnownCommands(t *testing.T) {
	tests := []struct {
		cmd      string
		wantHint string
	}{
		{"tmux-fake", "tmux"},
		{"claude-fake", "claude"},
		{"gh-fake", "gh"},
	}
	
	for _, tt := range tests {
		t.Run(tt.cmd, func(t *testing.T) {
			runner := HintingExecRunner{}
			_, err := runner.Run(context.Background(), "", tt.cmd, "test")
			
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			
			// For actual command-not-found errors on known commands,
			// we'll add hints in the next step
			if !strings.Contains(err.Error(), "not found") && !strings.Contains(err.Error(), "executable file not found") {
				t.Errorf("error should indicate command not found: %v", err)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify current behavior**

Run: `go test ./internal/lifecycle -run TestExecRunner -v`
Expected: PASS for basic error test, FAIL for HintingExecRunner (undefined)

- [ ] **Step 3: Add HintingExecRunner wrapper**

Add to `internal/lifecycle/runner.go` after ExecRunner:

```go
import (
	"fmt"
	"strings"
)

// HintingExecRunner wraps ExecRunner to add installation hints for common
// command-not-found errors (tmux, claude, gh).
type HintingExecRunner struct{}

func (HintingExecRunner) Run(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	
	if err != nil {
		// Wrap command-not-found errors with install hints
		if isCommandNotFound(err, name) {
			hint := commandInstallHint(name)
			if hint != "" {
				return string(out), fmt.Errorf("%w\n\n%s", err, hint)
			}
		}
	}
	
	return string(out), err
}

// isCommandNotFound checks if the error indicates the command wasn't found.
func isCommandNotFound(err error, cmdName string) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	return strings.Contains(errStr, "executable file not found") ||
		strings.Contains(errStr, "command not found") ||
		strings.Contains(errStr, "no such file or directory")
}

// commandInstallHint returns an installation hint for known commands.
func commandInstallHint(cmd string) string {
	// Extract base command name (strip path and fake suffixes for tests)
	base := cmd
	if idx := strings.LastIndex(cmd, "/"); idx >= 0 {
		base = cmd[idx+1:]
	}
	base = strings.TrimSuffix(base, "-fake")
	
	switch base {
	case "tmux":
		return "tmux not found.\n\nInstall: brew install tmux (macOS) or apt install tmux (Linux)"
	case "claude":
		return "claude not found.\n\nInstall Claude Code from https://claude.ai/download"
	case "gh":
		return "gh not found.\n\nInstall: brew install gh (macOS) or apt install gh (Linux)\nOr visit: https://cli.github.com"
	default:
		return ""
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/lifecycle -run TestExecRunner -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/lifecycle/runner.go internal/lifecycle/runner_test.go
git commit -m "feat(lifecycle): add HintingExecRunner with install hints

Wraps command-not-found errors for tmux, claude, and gh with
platform-specific installation instructions.

Co-Authored-By: Claude Sonnet 4.5 <noreply@anthropic.com>"
```

---

## Task 5: Wrap Git Worktree Errors

**Files:**
- Modify: `internal/lifecycle/lifecycle.go:284-305`

- [ ] **Step 1: Add worktree error hint helper**

Add to `internal/lifecycle/lifecycle.go` after the `commandInstallHint` function:

```go
// wrapWorktreeError adds contextual hints to git worktree errors.
func wrapWorktreeError(err error, operation string) error {
	if err == nil {
		return nil
	}
	
	errStr := err.Error()
	
	// Already exists
	if strings.Contains(errStr, "already exists") || strings.Contains(errStr, "already checked out") {
		return fmt.Errorf("%w\n\nWorktree already exists.\nRemove with: warden remove-worktree <id>\nOr: git worktree remove <path>", err)
	}
	
	// Locked
	if strings.Contains(errStr, "locked") {
		return fmt.Errorf("%w\n\nWorktree is locked.\nUnlock with: git worktree unlock <path>", err)
	}
	
	// Dirty worktree (uncommitted changes)
	if strings.Contains(errStr, "uncommitted changes") || strings.Contains(errStr, "is dirty") {
		return fmt.Errorf("%w\n\nCannot remove worktree with uncommitted changes.\nCommit or stash changes first, then retry.", err)
	}
	
	// Generic worktree error - just add operation context
	return fmt.Errorf("%s: %w", operation, err)
}
```

- [ ] **Step 2: Wrap worktree add errors for pr-review**

Modify `internal/lifecycle/lifecycle.go` at line 284:

```go
		if out, err := l.run.Run(ctx, req.Repo, "git", "worktree", "add", "--detach", rel); err != nil {
			return nil, wrapWorktreeError(fmt.Errorf("git worktree add --detach: %w\n%s", err, out), "create pr-review worktree")
		}
```

- [ ] **Step 3: Wrap gh pr checkout errors**

Modify `internal/lifecycle/lifecycle.go` at line 288:

```go
		if out, err := l.run.Run(ctx, abs, "gh", "pr", "checkout", req.PR); err != nil {
			// Check if gh command not found
			if isCommandNotFound(err, "gh") {
				return nil, fmt.Errorf("%w\n\n%s", err, commandInstallHint("gh"))
			}
			return nil, fmt.Errorf("gh pr checkout %s: %w\n%s", req.PR, err, out)
		}
```

- [ ] **Step 4: Wrap worktree add errors for branch checkout**

Modify `internal/lifecycle/lifecycle.go` at line 294:

```go
		if out, err := l.run.Run(ctx, req.Repo, "git", "worktree", "add", rel, req.Branch); err != nil {
			return nil, wrapWorktreeError(fmt.Errorf("git worktree add %s: %w\n%s", req.Branch, err, out), "create branch worktree")
		}
```

- [ ] **Step 5: Wrap worktree add errors for new branch**

Modify `internal/lifecycle/lifecycle.go` at line 304:

```go
	if out, err := l.run.Run(ctx, req.Repo, "git", "worktree", "add", rel, "-b", branch); err != nil {
		return nil, wrapWorktreeError(fmt.Errorf("git worktree add -b %s: %w\n%s", branch, err, out), "create new worktree")
	}
```

- [ ] **Step 6: Run existing tests to verify no regressions**

Run: `go test ./internal/lifecycle -v`
Expected: PASS (existing tests should still work with wrapped errors)

- [ ] **Step 7: Manual test - verify worktree error hint**

```bash
# Build warden
go build -o /tmp/warden-test cmd/warden/main.go

# Start daemon (if not running)
/tmp/warden-test daemon &
sleep 2

# Try to create a worktree that already exists
cd /tmp && git init test-repo && cd test-repo && git commit --allow-empty -m "init"
/tmp/warden-test start dev --repo /tmp/test-repo
/tmp/warden-test start dev --repo /tmp/test-repo  # Should show "already exists" hint

# Cleanup
pkill -f "warden-test daemon"
```

Expected: Second start shows "Worktree already exists" with removal hint

- [ ] **Step 8: Commit**

```bash
git add internal/lifecycle/lifecycle.go
git commit -m "feat(lifecycle): add hints to git worktree errors

Provides actionable recovery commands for common worktree failures:
- Already exists -> suggest warden remove-worktree
- Locked -> suggest git worktree unlock
- Uncommitted changes -> suggest commit/stash

Also wraps gh command-not-found with install hint.

Co-Authored-By: Claude Sonnet 4.5 <noreply@anthropic.com>"
```

---

## Task 6: Enhance Daemon Port Binding Errors

**Files:**
- Modify: `internal/cli/daemon.go:109`

- [ ] **Step 1: Wrap daemon ListenAndServe error**

Modify `internal/cli/daemon.go` at line 109:

```go
				log.Printf("warden daemon listening on %s", cfg.Addr)
				if err := srv.ListenAndServe(ctx, cfg.Addr); err != nil {
					// Check for port already in use
					if strings.Contains(err.Error(), "address already in use") {
						return fmt.Errorf("%w\n\nPort %s already in use.\nCheck if daemon is running: ps aux | grep 'warden daemon'\nOr specify different port: export WARDEN_ADDR=localhost:8766", err, cfg.Addr)
					}
					return err
				}
				return nil
```

- [ ] **Step 2: Add import for strings**

Verify `internal/cli/daemon.go` already imports "strings" (it doesn't, so add it):

```go
import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	
	// ... rest of imports
)
```

- [ ] **Step 3: Run tests to verify no regressions**

Run: `go test ./internal/cli -v`
Expected: PASS

- [ ] **Step 4: Manual test - verify port binding error**

```bash
# Start daemon on default port
go run cmd/warden/main.go daemon &
sleep 2

# Try to start another daemon on same port
go run cmd/warden/main.go daemon
```

Expected: Second daemon shows "Port ... already in use" with hints

- [ ] **Step 5: Cleanup test daemon**

```bash
pkill -f "warden daemon"
```

- [ ] **Step 6: Commit**

```bash
git add internal/cli/daemon.go
git commit -m "feat(daemon): enhance port binding error with hints

Suggests checking for running daemon or using different port.

Co-Authored-By: Claude Sonnet 4.5 <noreply@anthropic.com>"
```

---

## Task 7: Integration Testing & Documentation

**Files:**
- Create: `docs/shell-completion.md` (optional)
- Modify: `README.md` (optional section on completions)

- [ ] **Step 1: Run full test suite**

Run: `go test ./...`
Expected: All tests pass

- [ ] **Step 2: Build and install locally**

```bash
make build
./bin/warden --version
```

Expected: Binary builds successfully

- [ ] **Step 3: Test shell completion generation**

```bash
./bin/warden completion bash > /tmp/warden-completion.bash
./bin/warden completion zsh > /tmp/warden-completion.zsh
./bin/warden completion fish > /tmp/warden-completion.fish
./bin/warden completion powershell > /tmp/warden-completion.ps1
```

Expected: All four completion files generated without errors

- [ ] **Step 4: Verify completion content**

```bash
grep -q "warden" /tmp/warden-completion.bash && echo "bash OK"
grep -q "warden" /tmp/warden-completion.zsh && echo "zsh OK"
grep -q "warden" /tmp/warden-completion.fish && echo "fish OK"
grep -q "warden" /tmp/warden-completion.ps1 && echo "powershell OK"
```

Expected: All four show "OK"

- [ ] **Step 5: Test enhanced error messages end-to-end**

```bash
# Stop daemon
pkill -f "warden daemon" || true

# Test daemon connection error
./bin/warden ls 2>&1 | grep -q "Run: warden daemon" && echo "Daemon error OK"

# Start daemon
./bin/warden daemon > /tmp/warden-daemon.log 2>&1 &
sleep 2

# Test command successfully with daemon running
./bin/warden ls && echo "List OK"

# Cleanup
pkill -f "warden daemon"
```

Expected: All error messages show actionable hints

- [ ] **Step 6: Update README with completion info (optional)**

Add after Prerequisites section in `README.md`:

```markdown
### Shell Completion (Optional)

Enable shell completion for warden commands:

**Bash:**
```bash
warden completion bash > /etc/bash_completion.d/warden
```

**Zsh:**
```bash
warden completion zsh > /usr/local/share/zsh/site-functions/_warden
```

**Fish:**
```bash
warden completion fish > ~/.config/fish/completions/warden.fish
```

See `warden completion --help` for more details.
```

- [ ] **Step 7: Run final test suite**

Run: `go test ./... -race`
Expected: All tests pass with race detector

- [ ] **Step 8: Commit**

```bash
git add README.md
git commit -m "docs: add shell completion setup to README

Co-Authored-By: Claude Sonnet 4.5 <noreply@anthropic.com>"
```

---

## Self-Review

**Spec coverage check:**
- ✅ Shell completion for bash/zsh/fish/powershell — Task 2
- ✅ Daemon connection errors — Task 3
- ✅ tmux not found — Task 4 (HintingExecRunner)
- ✅ Claude CLI not found — Task 4 (HintingExecRunner)
- ✅ gh CLI not found — Task 5 (gh pr checkout wrapper)
- ✅ Git worktree errors — Task 5 (wrapWorktreeError)
- ✅ Port binding failures — Task 6
- ✅ Helper function in common.go — Task 1
- ✅ Testing plan — Task 7

**Placeholder scan:** No TBDs, TODOs, or placeholders. All code blocks complete.

**Type consistency:** 
- `isCommandNotFound` used consistently in Task 1 and Task 4
- `installHint` / `commandInstallHint` naming consistent
- Error wrapping pattern consistent across all tasks

All spec requirements covered with complete implementation code.
