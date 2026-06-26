package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// newHookRootGuardCmd is the PreToolUse root guard. warden installs it (via the
// per-agent `claude --settings` file) so a file-mutating tool call whose target
// lives in the MAIN repo working tree — the shared project root — is denied.
//
// Unlike the isolation guard (`hook guard`), this needs no daemon round-trip and
// no session state: the verdict is decided entirely from the target path plus a
// local `git rev-parse`. That makes it the backstop for the agents the isolation
// guard intentionally exempts — free-form and `--in-repo` agents that own no
// worktree — which the daemon policy leaves unconstrained. The trade-off is that
// it also overrides the `--in-repo` opt-out: with root_guard on, no spawned agent
// may write the main checkout. Operators who genuinely want an in-place agent set
// `wd config set root_guard false`.
//
// It ALWAYS exits 0 and fails open (allow) on any uncertainty — unreadable input,
// a non-git path, a path it cannot resolve, or a git error — because the guard is
// enforcement but must never wedge an agent on its own malfunction.
func newHookRootGuardCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "root-guard",
		Short: "PreToolUse main-worktree guard (reads hook JSON on stdin)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			raw, err := io.ReadAll(cmd.InOrStdin())
			if err != nil {
				return nil // can't read input → allow
			}
			var in preToolUseInput
			if err := json.Unmarshal(raw, &in); err != nil {
				return nil // unparseable → allow
			}
			reason := detectRootWrite(cmd.Context(), in.ToolName, toolInputPath(in.ToolInput), in.Cwd)
			if reason == "" {
				return nil // not a main-worktree write → allow
			}
			out, err := json.Marshal(preToolUseDecision{HookSpecificOutput: hookSpecificOutput{
				HookEventName:            "PreToolUse",
				PermissionDecision:       "deny",
				PermissionDecisionReason: reason,
			}})
			if err != nil {
				return nil
			}
			cmd.OutOrStdout().Write(out)
			return nil
		},
	}
}

// rootGuardTools are the file-mutating Claude tools the root guard evaluates,
// mirroring the isolation guard's set. The PreToolUse matcher in the generated
// settings already narrows to these; the map is a defensive re-check.
var rootGuardTools = map[string]bool{
	"Edit":         true,
	"Write":        true,
	"MultiEdit":    true,
	"NotebookEdit": true,
}

// detectRootWrite returns a deny message when a file-mutating tool call targets a
// path inside the main repo working tree, or "" to allow. The decision is local:
// it resolves the target to an absolute path (against cwd when relative), finds
// the nearest existing ancestor directory (a Write may create a not-yet-existing
// file or dir), and asks git whether that directory sits in the main worktree —
// the one whose git-dir equals its git-common-dir. A linked worktree (git-dir
// under .git/worktrees/...) differs from the common dir and is allowed.
func detectRootWrite(ctx context.Context, tool, path, cwd string) string {
	if !rootGuardTools[tool] || path == "" {
		return ""
	}
	abs := path
	if !filepath.IsAbs(abs) {
		if cwd == "" {
			return "" // unresolvable → allow
		}
		abs = filepath.Join(cwd, abs)
	}
	abs = filepath.Clean(abs)

	dir := nearestExistingDir(filepath.Dir(abs))
	if dir == "" {
		return ""
	}
	gitDir, commonDir, ok := gitDirs(ctx, dir)
	if !ok {
		return "" // not a git repo / git failed → allow
	}
	if filepath.Clean(gitDir) != filepath.Clean(commonDir) {
		return "" // linked worktree → allow
	}
	// Main worktree: its root is the parent of the common .git dir.
	root := filepath.Dir(filepath.Clean(commonDir))
	return "warden blocks edits to the main repo working tree. The target " + path +
		" is in the shared project root (" + root + "), not an isolated git worktree. " +
		"Spawned agents must make changes inside their own worktree — warden creates one " +
		"when you spawn with a managed type (`wd start <ticket> --type <type>`). Re-run the " +
		"edit against a path inside your worktree. (If this agent is meant to work in-place, " +
		"the operator can disable this with `wd config set root_guard false`.)"
}

// nearestExistingDir walks up from dir to the first directory that exists,
// returning "" if it runs off the filesystem root without finding one. A Write
// can name a file in a directory that does not exist yet, so the guard resolves
// the repo from the closest existing ancestor instead of the literal target dir.
func nearestExistingDir(dir string) string {
	for {
		if dir == "" {
			return ""
		}
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "" // reached the root without an existing dir
		}
		dir = parent
	}
}

// gitDirs runs `git -C dir rev-parse --git-dir --git-common-dir` and returns both
// paths resolved to absolute (git prints them relative to dir), plus ok=false
// when dir is not in a git repo or git is unavailable. The two values are equal
// in the main worktree and differ in a linked worktree, which is the whole signal
// the guard needs.
func gitDirs(ctx context.Context, dir string) (gitDir, commonDir string, ok bool) {
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--git-dir", "--git-common-dir").Output()
	if err != nil {
		return "", "", false
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		return "", "", false
	}
	return absFrom(dir, lines[0]), absFrom(dir, lines[1]), true
}

// absFrom resolves a git-printed path against dir (git prints paths relative to
// its working directory, which is dir here) and returns it absolute and cleaned.
func absFrom(dir, p string) string {
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	return filepath.Clean(p)
}
