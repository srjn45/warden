package cli

import (
	"encoding/json"
	"io"
	"sort"

	"github.com/spf13/cobra"
	"github.com/srjn45/warden/internal/lifecycle"
)

// newHookCheckGuardCmd is the PreToolUse check-redirect hook. warden installs it
// (via the per-agent `claude --settings` file) so a raw test/lint/build command
// the project registered in .warden/check.yml — `go test ./...`, `make verify`,
// `npm test`, … — is denied with a message pointing the agent at `wd check`,
// which returns only the failures instead of the full log. Every other command
// passes through untouched.
//
// Unlike git's closed vocabulary, test commands are open-ended, so the hook never
// guesses: it redirects ONLY what the per-project config registers. It reads that
// config directly from the agent's cwd (no daemon round-trip) — the same parsed
// .warden/check.yml the runner uses, so the gate and the runner can't drift — and
// ALWAYS exits 0, failing open (allow) on any error or when the repo has no
// config. The hook is enforcement, but it must never wedge an agent.
func newHookCheckGuardCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "check-guard",
		Short: "PreToolUse check-redirect guard (reads hook JSON on stdin)",
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
				return nil // only Bash carries raw test commands
			}
			dir := in.Cwd
			if dir == "" {
				dir = "." // no cwd reported → resolve config from the process dir
			}
			checks, err := lifecycle.CheckCommands(dir)
			if err != nil || len(checks) == 0 {
				return nil // no config / unreadable → redirect nothing (pass through)
			}
			name, registered := detectCheckRedirect(toolInputCommand(in.ToolInput), checks)
			if name == "" {
				return nil // not a registered check command → allow
			}
			out, err := json.Marshal(preToolUseDecision{HookSpecificOutput: hookSpecificOutput{
				HookEventName:            "PreToolUse",
				PermissionDecision:       "deny",
				PermissionDecisionReason: checkRedirectMsg(name, registered),
			}})
			if err != nil {
				return nil
			}
			cmd.OutOrStdout().Write(out)
			return nil
		},
	}
}

// checkRedirectMsg composes the deny reason. It names the exact replacement tool
// and the matched check so Claude can re-issue the action without guessing, and
// spells out the escape hatch so it does not flail trying to force the raw run.
func checkRedirectMsg(name, cmd string) string {
	return "Use the warden tool mcp__warden__check (or `wd check " + name + "`) to run this project's checks " +
		"instead of raw `" + cmd + "`. warden runs the configured command and returns only the failing output, " +
		"not the full log, and links the run to this agent. To run a focused subset (e.g. a single test), run " +
		"that directly — only the registered check command is redirected."
}

// detectCheckRedirect scans a Bash command for a raw invocation of a check the
// project registered in .warden/check.yml and returns the matched check's name
// and configured command, or ("", "") when the command runs no such check.
//
// A registered command matches when its leading simple-command tokens are a
// prefix of one of the Bash command's simple commands — so `go test ./...` and
// `go test ./... -count=1` both redirect, while a narrower `go test -run X ./pkg`
// (which `wd check` cannot reproduce) does not. The command string is split into
// simple commands honouring quotes, so a check name mentioned inside a quoted
// argument is not a false positive, and leading `NAME=value` env assignments are
// skipped on both sides. Matching is config-driven only — warden never guesses
// test vocabulary, so a repo with no config redirects nothing.
func detectCheckRedirect(command string, checks map[string]string) (name, cmd string) {
	type signature struct {
		name, cmd string
		words     []string
	}
	// Build the signatures in stable name order so the chosen redirect is
	// deterministic when more than one registered check could match a command.
	regNames := make([]string, 0, len(checks))
	for n := range checks {
		regNames = append(regNames, n)
	}
	sort.Strings(regNames)
	sigs := make([]signature, 0, len(checks))
	for _, n := range regNames {
		words := checkSignature(checks[n])
		if len(words) == 0 {
			continue // empty / unparseable registered command — nothing to match
		}
		sigs = append(sigs, signature{name: n, cmd: checks[n], words: words})
	}

	for _, bash := range parseCommands(command) {
		bash = skipEnvAssignments(bash)
		for _, s := range sigs {
			if hasPrefixTokens(bash, s.words) {
				return s.name, s.cmd
			}
		}
	}
	return "", ""
}

// checkSignature returns the tokens of a registered command's first simple
// command with leading env assignments stripped — the sequence a Bash command
// must lead with to be recognised as that check.
func checkSignature(cmd string) []string {
	cmds := parseCommands(cmd)
	if len(cmds) == 0 {
		return nil
	}
	return skipEnvAssignments(cmds[0])
}

// skipEnvAssignments drops leading `NAME=value` env-assignment tokens.
func skipEnvAssignments(words []string) []string {
	i := 0
	for i < len(words) && isEnvAssignment(words[i]) {
		i++
	}
	return words[i:]
}

// hasPrefixTokens reports whether sig is a non-empty prefix of words.
func hasPrefixTokens(words, sig []string) bool {
	if len(sig) == 0 || len(words) < len(sig) {
		return false
	}
	for i := range sig {
		if words[i] != sig[i] {
			return false
		}
	}
	return true
}
