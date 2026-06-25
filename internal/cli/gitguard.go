package cli

import (
	"encoding/json"
	"io"
	"strings"

	"github.com/spf13/cobra"
)

// newHookGitGuardCmd is the PreToolUse git-redirect hook. warden installs it (via
// the per-agent `claude --settings` file) so a raw git mutation run through Bash —
// `git commit`, `git push`, `git pull`, `git rebase` — is denied with a message
// naming the warden tool that replaces it. Read-only git (status/log/diff/show…)
// and every non-git command pass through untouched. `git rebase --continue` (and
// the other in-progress-rebase controls) is also allowed, so an agent that hits
// a conflict during `wd sync` can finish the rebase the tool told it to.
//
// Unlike the isolation guard this needs no daemon round-trip: the redirect is a
// static mapping, so it is decided entirely from the command string. It ALWAYS
// exits 0 and fails open (allow) on any error — the hook is enforcement, but it
// must never wedge an agent because its own input was unreadable.
func newHookGitGuardCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "git-guard",
		Short: "PreToolUse git-redirect guard (reads hook JSON on stdin)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			raw, err := io.ReadAll(cmd.InOrStdin())
			if err != nil {
				return nil // can't read input → allow
			}
			var in preToolUseInput
			if err := json.Unmarshal(raw, &in); err != nil {
				return nil // unparseable → allow
			}
			if in.ToolName != "Bash" {
				return nil // only Bash carries raw git commands
			}
			reason := detectGitRedirect(toolInputCommand(in.ToolInput))
			if reason == "" {
				return nil // no raw git mutation → allow
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

// toolInputCommand pulls the shell command from a Bash tool_input object.
func toolInputCommand(raw json.RawMessage) string {
	var ti struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(raw, &ti); err != nil {
		return ""
	}
	return ti.Command
}

// gitRedirects maps a mutating git subcommand to the deny message naming its
// warden replacement. Subcommands absent here (status, log, diff, show, branch,
// fetch, add, …) are read-only or harmless and are never redirected.
var gitRedirects = map[string]string{
	"commit": gitRedirectMsg("git commit", "mcp__warden__commit (or `wd commit -m <msg>`)", "stage and commit your changes"),
	"push":   gitRedirectMsg("git push", "mcp__warden__push (or `wd push`)", "push your branch"),
	"pull":   gitRedirectMsg("git pull", "mcp__warden__sync (or `wd sync`)", "rebase onto the base branch"),
	"rebase": gitRedirectMsg("git rebase", "mcp__warden__sync (or `wd sync`)", "rebase onto the base branch"),
}

// gitRedirectMsg composes the deny reason. It names the exact replacement tool so
// Claude can re-issue the action without guessing.
func gitRedirectMsg(raw, tool, action string) string {
	return "Use the warden tool " + tool + " to " + action + " instead of raw `" + raw +
		"`. warden enforces the branch rail (never commits to main), runs pre-commit " +
		"hooks and returns only failures, and links the action to this agent. " +
		"Read-only git (status, log, diff, show, branch) stays available — run it directly."
}

// detectGitRedirect scans a Bash command string for a raw git mutation and
// returns the redirect message for the first one found, or "" when the command
// runs no such mutation. It splits the string into individual commands (honouring
// quotes so a mutation named inside a quoted argument or commit message is not a
// false positive) and inspects each for a `git <mutating-subcommand>` invocation.
func detectGitRedirect(command string) string {
	for _, words := range parseCommands(command) {
		sub := gitSubcommand(words)
		// `git rebase --continue|--abort|--skip|…` manages an in-progress rebase
		// (one wd sync itself started and tells you to finish) rather than
		// kicking off a new one, so it must pass through — otherwise the guard
		// wedges the agent mid-rebase with no sanctioned way out.
		if sub == "rebase" && isRebaseControl(words) {
			continue
		}
		if msg, ok := gitRedirects[sub]; ok {
			return msg
		}
	}
	return ""
}

// rebaseControlFlags are the `git rebase` options that operate on an already
// in-progress rebase instead of starting a fresh one.
var rebaseControlFlags = map[string]bool{
	"--continue": true, "--abort": true, "--skip": true,
	"--quit": true, "--edit-todo": true, "--show-current-patch": true,
}

// isRebaseControl reports whether a `git rebase …` invocation carries a control
// flag (--continue/--abort/…) that drives an in-progress rebase rather than
// starting a new one.
func isRebaseControl(words []string) bool {
	for _, w := range words {
		if rebaseControlFlags[w] {
			return true
		}
	}
	return false
}

// gitGlobalWithValue lists the git global options that consume the following
// token as their value, so the subcommand scan can skip past them
// (e.g. `git -C path commit` → subcommand is "commit", not "path").
var gitGlobalWithValue = map[string]bool{
	"-C": true, "-c": true,
	"--git-dir": true, "--work-tree": true, "--namespace": true, "--exec-path": true,
}

// gitSubcommand returns the lowercased git subcommand for a single command's
// tokens, or "" when the command does not invoke git. Leading `NAME=value`
// environment assignments are skipped, as are git global options (with their
// values), so the first real positional token after `git` is the subcommand.
func gitSubcommand(words []string) string {
	i := 0
	for i < len(words) && isEnvAssignment(words[i]) {
		i++
	}
	if i >= len(words) || !isGitWord(words[i]) {
		return ""
	}
	for i++; i < len(words); i++ {
		w := words[i]
		if !strings.HasPrefix(w, "-") {
			return strings.ToLower(w) // first non-option token is the subcommand
		}
		if gitGlobalWithValue[w] {
			i++ // also skip the option's value
		}
	}
	return ""
}

// isGitWord reports whether tok invokes the git binary, either bare (`git`) or by
// path (`/usr/bin/git`).
func isGitWord(tok string) bool {
	return tok == "git" || strings.HasSuffix(tok, "/git")
}

// isEnvAssignment reports whether tok is a leading `NAME=value` env assignment.
func isEnvAssignment(tok string) bool {
	eq := strings.IndexByte(tok, '=')
	if eq <= 0 {
		return false
	}
	for i := 0; i < eq; i++ {
		c := tok[i]
		if c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (i > 0 && c >= '0' && c <= '9') {
			continue
		}
		return false
	}
	return true
}

// parseCommands splits a shell command string into its individual commands, each
// as a list of tokens. It is a deliberately small tokenizer — enough to keep
// quoted text intact and to break on the operators that separate commands
// (`&&`, `||`, `|`, `;`, `&`, newline) and subshell parens — not a full shell
// parser. Quoting matters: it stops a mutation named inside a quoted argument
// (e.g. `git commit -m "also git push later"`) from being read as a command.
func parseCommands(s string) [][]string {
	var cmds [][]string
	var cur []string
	var tok strings.Builder
	hasTok := false
	var quote byte // 0, '\'' or '"'

	flushTok := func() {
		if hasTok {
			cur = append(cur, tok.String())
			tok.Reset()
			hasTok = false
		}
	}
	flushCmd := func() {
		flushTok()
		if len(cur) > 0 {
			cmds = append(cmds, cur)
			cur = nil
		}
	}

	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			} else {
				tok.WriteByte(c)
				hasTok = true
			}
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
			hasTok = true // empty quotes ("") are still a token
		case '\\':
			if i+1 < len(s) {
				i++
				tok.WriteByte(s[i])
				hasTok = true
			}
		case ' ', '\t', '\r':
			flushTok()
		case ';', '&', '|', '\n', '(', ')':
			flushCmd()
		case '<', '>':
			flushTok() // redirection separates the token but not the command
		default:
			tok.WriteByte(c)
			hasTok = true
		}
	}
	flushCmd()
	return cmds
}
