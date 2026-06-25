package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDetectGitRedirectMutations(t *testing.T) {
	cases := []struct {
		name, cmd, wantTool string
	}{
		{"commit", `git commit -m "do x"`, "mcp__warden__commit"},
		{"push", "git push", "mcp__warden__push"},
		{"push upstream", "git push -u origin feat", "mcp__warden__push"},
		{"pull", "git pull origin main", "mcp__warden__sync"},
		{"rebase", "git rebase origin/main", "mcp__warden__sync"},
		{"after cd", `cd /repo && git commit -m x`, "mcp__warden__commit"},
		{"after add", `git add -A && git commit -m x`, "mcp__warden__commit"},
		{"global -C", "git -C /repo commit -m x", "mcp__warden__commit"},
		{"global -c", `git -c user.name=bot commit -m x`, "mcp__warden__commit"},
		{"by path", "/usr/bin/git push", "mcp__warden__push"},
		{"env prefix", "GIT_AUTHOR_NAME=bot git commit -m x", "mcp__warden__commit"},
		{"piped second", `echo hi | git commit -m x`, "mcp__warden__commit"},
		{"subshell", `echo $(git push)`, "mcp__warden__push"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := detectGitRedirect(tc.cmd)
			require.NotEmpty(t, msg, "expected a redirect for %q", tc.cmd)
			require.Contains(t, msg, tc.wantTool)
		})
	}
}

func TestDetectGitRedirectAllowsReadsAndNonGit(t *testing.T) {
	allowed := []string{
		"git status",
		"git log --oneline -5",
		"git diff HEAD~1",
		"git show abc123",
		"git branch -a",
		"git fetch origin",
		"git add -A",
		"git rebase --continue", // finishing an in-progress rebase, not starting one
		"git rebase --abort",
		"git rebase --skip",
		"git rebase --quit",
		"git rebase --edit-todo",
		"GIT_EDITOR=true git rebase --continue",
		"git -C /repo rebase --continue",
		"ls -la",
		"make test",
		`echo "git push"`, // mutation only inside a quoted string
		"go test ./...",
		"",
		"git", // bare git, no subcommand
	}
	for _, cmd := range allowed {
		require.Empty(t, detectGitRedirect(cmd), "should allow %q", cmd)
	}
}

// quoted commit message containing && must not be split into a fake second command.
func TestDetectGitRedirectQuotedOperatorIsNotSplit(t *testing.T) {
	require.Contains(t, detectGitRedirect(`git commit -m "a && git push"`), "mcp__warden__commit")
}

func TestGitSubcommandSkipsGlobalFlagValues(t *testing.T) {
	require.Equal(t, "commit", gitSubcommand([]string{"git", "-C", "/repo", "-c", "k=v", "commit"}))
	require.Equal(t, "", gitSubcommand([]string{"echo", "git", "commit"}))
	require.Equal(t, "", gitSubcommand([]string{"git"}))
}

// runGitGuardHook drives the `hook git-guard` command with the given stdin JSON.
func runGitGuardHook(t *testing.T, stdin string) string {
	t.Helper()
	cmd := newHookGitGuardCmd()
	cmd.SetIn(strings.NewReader(stdin))
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	require.NoError(t, cmd.Execute())
	return out.String()
}

func TestHookGitGuardDeniesCommit(t *testing.T) {
	in := `{"tool_name":"Bash","tool_input":{"command":"git commit -m x"}}`
	out := runGitGuardHook(t, in)
	var dec preToolUseDecision
	require.NoError(t, json.Unmarshal([]byte(out), &dec))
	require.Equal(t, "deny", dec.HookSpecificOutput.PermissionDecision)
	require.Equal(t, "PreToolUse", dec.HookSpecificOutput.HookEventName)
	require.Contains(t, dec.HookSpecificOutput.PermissionDecisionReason, "mcp__warden__commit")
}

func TestHookGitGuardAllowsRead(t *testing.T) {
	out := runGitGuardHook(t, `{"tool_name":"Bash","tool_input":{"command":"git status"}}`)
	require.Empty(t, out, "a read-only git command emits nothing (allow)")
}

func TestHookGitGuardIgnoresNonBash(t *testing.T) {
	out := runGitGuardHook(t, `{"tool_name":"Edit","tool_input":{"file_path":"/x"}}`)
	require.Empty(t, out, "non-Bash tools are not this hook's concern")
}

func TestHookGitGuardFailsOpenOnGarbage(t *testing.T) {
	require.Empty(t, runGitGuardHook(t, "not json"))
}
