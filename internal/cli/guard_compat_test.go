package cli

// COMPATIBILITY SUITE — git guard + legacy hook paths (task t1-compat-baseline).
//
// Pins the PreToolUse hook protocol that installed agents depend on: the legacy
// `warden hook <name>` paths must keep resolving, keep their stdin JSON → stdout
// JSON contract, and keep failing open (exit 0, empty stdout) on any error. Also
// pins that the git guard lets in-progress rebase controls through.
//
// Later tasks may only change an assertion in this file when their task prompt
// says so, and must say why in the commit message.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// runLegacyHook resolves `warden hook <name>` through the real cobra tree and
// drives it with stdin, returning stdout and the Execute error.
func runLegacyHook(t *testing.T, name, stdin string) (string, error) {
	t.Helper()
	root := newRootCmd()
	found, _, err := root.Find([]string{"hook", name})
	require.NoError(t, err)
	require.Equal(t, name, found.Name(), "legacy hook path must resolve to its own command")
	var out, errb bytes.Buffer
	root.SetIn(strings.NewReader(stdin))
	root.SetOut(&out)
	root.SetErr(&errb)
	root.SetArgs([]string{"hook", name})
	err = root.Execute()
	return out.String(), err
}

func TestCompatLegacyHookPathsResolve(t *testing.T) {
	root := newRootCmd()
	for _, name := range []string{"git-guard", "check-guard", "guard", "root-guard"} {
		found, rest, err := root.Find([]string{"hook", name})
		require.NoError(t, err, name)
		require.Equal(t, name, found.Name(), name)
		require.Empty(t, rest, name)
		require.Equal(t, "hook", found.Parent().Name(), name)
	}
	hook, _, err := root.Find([]string{"hook"})
	require.NoError(t, err)
	require.True(t, hook.Hidden, "hook namespace stays hidden")
}

func TestCompatGitGuardExactDenyJSON(t *testing.T) {
	out, err := runLegacyHook(t, "git-guard", `{"tool_name":"Bash","tool_input":{"command":"git commit -m x"}}`)
	require.NoError(t, err)
	const want = `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"Use the warden tool mcp__warden__commit (or ` + "`wd commit -m \\u003cmsg\\u003e`" + `) to stage and commit your changes instead of raw ` + "`git commit`" + `. warden enforces the branch rail (never commits to main), runs pre-commit hooks and returns only failures, and links the action to this agent. Read-only git (status, log, diff, show, branch) stays available — run it directly."}}`
	require.Equal(t, want, strings.TrimSpace(out))
}

func TestCompatGitGuardDeniedToolMapping(t *testing.T) {
	for cmd, tool := range map[string]string{
		"git commit -m x":        "mcp__warden__commit",
		"git push":               "mcp__warden__push",
		"git pull":               "mcp__warden__sync",
		"git rebase origin/main": "mcp__warden__sync",
	} {
		msg := detectGitRedirect(cmd)
		require.True(t, strings.HasPrefix(msg, "Use the warden tool "+tool+" "), "%s: %s", cmd, msg)
		require.Contains(t, msg, " instead of raw `git ", cmd)
	}
}

// git rebase --continue/--abort/--skip must be ALLOWED (empty stdout, exit 0):
// wd sync leaves a conflicted rebase in progress and tells the agent to finish it.
func TestCompatGitGuardAllowsRebaseControls(t *testing.T) {
	for _, flag := range []string{"--continue", "--abort", "--skip"} {
		for _, cmd := range []string{
			"git rebase " + flag,
			"GIT_EDITOR=true git rebase " + flag,
			"git -C /some/wt rebase " + flag,
			"cd /wt && git rebase " + flag,
		} {
			stdin := `{"tool_name":"Bash","tool_input":{"command":` + jsonString(cmd) + `}}`
			out, err := runLegacyHook(t, "git-guard", stdin)
			require.NoError(t, err, cmd)
			require.Empty(t, out, "verdict for %q must be allow (no output)", cmd)
		}
	}
	// ...but a fresh rebase is still denied, even when chained after a control flag.
	out, err := runLegacyHook(t, "git-guard", `{"tool_name":"Bash","tool_input":{"command":"git rebase --continue && git rebase origin/main"}}`)
	require.NoError(t, err)
	require.Contains(t, out, `"permissionDecision":"deny"`)
	require.Contains(t, out, "mcp__warden__sync")
}

func TestCompatHookFailOpenProtocol(t *testing.T) {
	for _, name := range []string{"git-guard", "check-guard", "guard", "root-guard"} {
		for _, stdin := range []string{"", "not json", "{", `{"tool_name":"Bash"}`, `{"tool_name":"Read","tool_input":{}}`} {
			out, err := runLegacyHook(t, name, stdin)
			require.NoError(t, err, "%s must exit 0 on %q", name, stdin)
			require.Empty(t, out, "%s must allow (empty stdout) on %q", name, stdin)
		}
	}
}

func TestCompatCheckGuardProtocolRealConfig(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".warden"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".warden", "check.yml"), []byte("check:\n  test: \"go test ./...\"\n"), 0o644))

	out, err := runLegacyHook(t, "check-guard", `{"tool_name":"Bash","cwd":`+jsonString(dir)+`,"tool_input":{"command":"go test ./..."}}`)
	require.NoError(t, err)
	const want = `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"Use the warden tool mcp__warden__check (or ` + "`wd check test`" + `) to run this project's checks instead of raw ` + "`go test ./...`" + `. warden runs the configured command and returns only the failing output, not the full log, and links the run to this agent. To run a focused subset (e.g. a single test), run that directly — only the registered check command is redirected."}}`
	require.Equal(t, want, strings.TrimSpace(out))

	// Not a registered command → allow.
	out, err = runLegacyHook(t, "check-guard", `{"tool_name":"Bash","cwd":`+jsonString(dir)+`,"tool_input":{"command":"go test ./pkg -run X"}}`)
	require.NoError(t, err)
	require.Empty(t, out)

	// No config in cwd → allow (fail open).
	out, err = runLegacyHook(t, "check-guard", `{"tool_name":"Bash","cwd":`+jsonString(t.TempDir())+`,"tool_input":{"command":"go test ./..."}}`)
	require.NoError(t, err)
	require.Empty(t, out)
}

func TestCompatRootGuardProtocolRealRepo(t *testing.T) {
	main, linked := initRepoWithWorktree(t)

	out, err := runLegacyHook(t, "root-guard", `{"tool_name":"Edit","cwd":`+jsonString(main)+`,"tool_input":{"file_path":`+jsonString(filepath.Join(main, "seed.txt"))+`}}`)
	require.NoError(t, err)
	require.Contains(t, out, `"hookEventName":"PreToolUse"`)
	require.Contains(t, out, `"permissionDecision":"deny"`)
	require.Contains(t, out, "main repo working tree")

	out, err = runLegacyHook(t, "root-guard", `{"tool_name":"Edit","cwd":`+jsonString(linked)+`,"tool_input":{"file_path":`+jsonString(filepath.Join(linked, "seed.txt"))+`}}`)
	require.NoError(t, err)
	require.Empty(t, out, "linked worktree edits are allowed")
}

// guard (isolation) talks to the daemon; with no daemon reachable it must fail open.
func TestCompatIsolationGuardFailsOpenWithoutDaemon(t *testing.T) {
	t.Setenv("WARDEN_SESSION_ID", "")
	out, err := runLegacyHook(t, "guard", `{"tool_name":"Edit","tool_input":{"file_path":"/etc/x"}}`)
	require.NoError(t, err)
	require.Empty(t, out)
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
