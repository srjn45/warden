# UX Improvements: Shell Completion & Enhanced Error Messages

**Date:** 2026-06-12  
**Status:** Approved for implementation

## Overview

Two quick-win UX improvements for warden:

1. **Shell completion support** — Add Cobra's built-in completion command to generate shell completions for bash/zsh/fish/powershell
2. **Enhanced error messages** — Add actionable hints to common error paths throughout the codebase

## Goals

- Improve discoverability of warden commands through shell completion
- Reduce friction when users encounter common errors by providing immediate, actionable solutions
- Maintain consistency with existing error handling patterns
- Keep implementation simple and maintainable

## Non-Goals

- Building a custom completion system (use Cobra's built-in generators)
- Creating a complex error handling framework with error catalogs
- Changing existing error types or error handling patterns beyond adding hints

## Design

### 1. Shell Completion Command

#### Architecture

Add a `completion` subcommand to the root command that generates shell completion scripts using Cobra's built-in generators.

#### Implementation Details

**New file:** `internal/cli/completion.go`

The completion command will have four subcommands:
- `bash` — generates bash completion script
- `zsh` — generates zsh completion script  
- `fish` — generates fish completion script
- `powershell` — generates PowerShell completion script

Each subcommand:
- Calls the appropriate Cobra generator method (`GenBashCompletion`, `GenZshCompletion`, etc.)
- Writes the completion script to stdout
- Includes installation instructions in the command's `Long` description

**Registration:** Add to `root.go` via `root.AddCommand(newCompletionCmd())`

#### User Experience

```bash
# Generate and install bash completion
warden completion bash > /etc/bash_completion.d/warden

# Generate and install zsh completion  
warden completion zsh > /usr/local/share/zsh/site-functions/_warden

# Generate and install fish completion
warden completion fish > ~/.config/fish/completions/warden.fish

# Generate and install PowerShell completion
warden completion powershell > warden.ps1
```

The command's help text will include these examples for easy copy-paste.

### 2. Enhanced Error Messages

#### Architecture

Enhance error messages at the point where they occur by wrapping errors with actionable hints. No new abstractions or error types — just augment existing error returns with contextual information.

#### Error Sites and Enhancements

##### 1. Daemon Connection Failures
**Location:** `internal/client/client.go`

**Current:** `"daemon not running — start it with 'warden daemon' (or via launchd)"`

**Enhancement:** Improve the existing `ErrDaemonDown` message to include installation script reference:
```
daemon not running

Run: warden daemon
Or install as a service: ./scripts/install.sh
```

**Detection:** Check for `syscall.ECONNREFUSED` and timeout errors in the HTTP client.

##### 2. tmux Not Found
**Location:** `internal/lifecycle/lifecycle.go` (and anywhere tmux commands are executed)

**Enhancement:** Wrap exec errors when running tmux commands:
```
tmux not found

Install: brew install tmux (macOS) or apt install tmux (Linux)
```

**Detection:** Check `exec.Error` with `errors.Is(err, exec.ErrNotFound)` or check for "executable file not found" in error message.

##### 3. Claude CLI Not Found
**Location:** `internal/lifecycle/lifecycle.go`

**Enhancement:** Wrap exec errors when running claude commands:
```
claude not found

Install Claude Code from https://claude.ai/download
```

**Detection:** Same as tmux — check for command not found errors.

##### 4. gh CLI Not Found
**Location:** `internal/lifecycle/lifecycle.go` (when spawning pr-review sessions)

**Enhancement:** Wrap exec errors when running gh commands:
```
gh not found

Install: brew install gh (macOS) or apt install gh (Linux)
Or visit: https://cli.github.com
```

**Detection:** Same as above — check for command not found errors.

##### 5. Git Worktree Errors
**Location:** `internal/lifecycle/lifecycle.go` and daemon handlers for worktree operations

**Enhancement:** Provide context-specific hints based on the git error:

- **Worktree already exists:**
  ```
  Worktree already exists
  
  Remove with: warden remove-worktree <id>
  Or: git worktree remove <path>
  ```

- **Worktree locked:**
  ```
  Worktree is locked
  
  Unlock with: git worktree unlock <path>
  ```

- **Dirty worktree (uncommitted changes):**
  ```
  Cannot remove worktree with uncommitted changes
  
  Commit or stash changes first, then retry
  ```

**Detection:** Parse git command stderr output for specific error patterns.

##### 6. Port Binding Failures
**Location:** Daemon startup code in `internal/daemon/`

**Enhancement:** When daemon fails to bind to port 8765:
```
Port 8765 already in use

Check if daemon is running: ps aux | grep 'warden daemon'
Or specify different port: export WARDEN_ADDR=localhost:8766
```

**Detection:** Check for "address already in use" error when starting HTTP server.

##### 7. Permission Errors
**Location:** `internal/client/client.go`

**Enhancement:** When HTTP requests fail with permission denied:
```
Permission denied accessing daemon

Check that the daemon is running as your user
```

**Detection:** Check for `syscall.EACCES` errors in HTTP client.

#### Implementation Approach

1. **Add helper function in `internal/cli/common.go`:**
   ```go
   // isCommandNotFound checks if an error indicates a command was not found
   func isCommandNotFound(err error) bool
   ```

2. **Wrap errors inline** where they occur using:
   ```go
   return fmt.Errorf("%w\n\nHint: %s", err, hint)
   ```

3. **Platform detection:** Use `runtime.GOOS` for platform-specific install hints where relevant.

4. **Keep hints concise:** One or two lines maximum, focus on immediate actionable steps.

## Testing Plan

### Shell Completion
- Generate completion scripts for all four shells
- Verify they contain expected warden commands
- Test installation in bash and zsh (manual verification)
- Verify completion works for subcommands and flags

### Error Messages
- Unit tests for `isCommandNotFound` helper
- Integration tests that trigger each error path:
  - Stop daemon, verify enhanced connection error
  - Mock missing binaries (tmux, claude, gh) and verify hints
  - Trigger worktree errors and verify contextual hints
  - Attempt to start daemon on occupied port
- Manual testing of error message formatting and readability

## Documentation Updates

- Update `README.md` to mention shell completion setup
- Add shell completion section to `docs/USAGE.md` if it exists
- No changes needed for error messages (self-documenting via hints)

## Success Criteria

1. Users can generate and install shell completions for bash, zsh, fish, and PowerShell
2. Common error scenarios provide immediate, actionable guidance
3. Error hints are platform-appropriate (macOS vs Linux install commands)
4. All error message changes maintain backward compatibility
5. Tests pass for both features

## Open Questions

None — design is approved and ready for implementation.
