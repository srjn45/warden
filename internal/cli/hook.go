package cli

import (
	"context"
	"encoding/json"
	"io"
	"time"

	"github.com/spf13/cobra"
)

// guardTimeout caps the daemon round-trip on every file-mutating tool call. The
// hook runs inline before each Edit/Write, so a slow or absent daemon must fail
// open quickly rather than stall the agent.
const guardTimeout = 3 * time.Second

// preToolUseInput is the subset of Claude Code's PreToolUse hook stdin we read.
// tool_input is left raw so we can pull whichever path key the tool uses; cwd is
// the agent's working dir, used by the check guard to locate the project's
// .warden/check.yml.
type preToolUseInput struct {
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input"`
	Cwd       string          `json:"cwd"`
}

// preToolUseDecision is the hook's stdout contract for blocking a tool call.
// Emitting nothing (exit 0) means allow, so we only marshal this on a deny.
type preToolUseDecision struct {
	HookSpecificOutput hookSpecificOutput `json:"hookSpecificOutput"`
}

type hookSpecificOutput struct {
	HookEventName            string `json:"hookEventName"`
	PermissionDecision       string `json:"permissionDecision"`
	PermissionDecisionReason string `json:"permissionDecisionReason"`
}

func newHookCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "hook",
		Short:  "Internal hooks invoked by Claude Code (not for direct use)",
		Hidden: true,
	}
	cmd.AddCommand(newHookGuardCmd())
	cmd.AddCommand(newHookGitGuardCmd())
	cmd.AddCommand(newHookCheckGuardCmd())
	return cmd
}

// newHookGuardCmd is the PreToolUse isolation guard. warden installs it (via a
// per-agent `claude --settings` file) so a file-mutating tool call is checked
// against the daemon's worktree-isolation policy: an edit that escapes the
// agent's worktree into the shared repo is denied with a redirect message Claude
// can act on. It ALWAYS exits 0 and fails open (allow) on any error — the guard
// is a backstop and must never wedge an agent because the daemon is busy or down.
func newHookGuardCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "guard",
		Short: "PreToolUse isolation guard (reads hook JSON on stdin)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			raw, err := io.ReadAll(cmd.InOrStdin())
			if err != nil {
				return nil // can't read input → allow
			}
			var in preToolUseInput
			if err := json.Unmarshal(raw, &in); err != nil {
				return nil // unparseable → allow
			}
			path := toolInputPath(in.ToolInput)
			session := envID("SESSION_ID")
			if path == "" || session == "" {
				return nil // nothing to evaluate → allow
			}

			ctx, cancel := context.WithTimeout(cmd.Context(), guardTimeout)
			defer cancel()
			v, err := clientFor(cmd).Guard(ctx, session, in.ToolName, path)
			if err != nil || v.Decision != "deny" {
				return nil // daemon error or allow → allow
			}

			out, err := json.Marshal(preToolUseDecision{HookSpecificOutput: hookSpecificOutput{
				HookEventName:            "PreToolUse",
				PermissionDecision:       "deny",
				PermissionDecisionReason: v.Reason,
			}})
			if err != nil {
				return nil
			}
			cmd.OutOrStdout().Write(out)
			return nil
		},
	}
}

// toolInputPath pulls the target path from a tool_input object, trying the
// file-tools key (file_path) then the notebook key (notebook_path).
func toolInputPath(raw json.RawMessage) string {
	var ti struct {
		FilePath     string `json:"file_path"`
		NotebookPath string `json:"notebook_path"`
	}
	if err := json.Unmarshal(raw, &ti); err != nil {
		return ""
	}
	if ti.FilePath != "" {
		return ti.FilePath
	}
	return ti.NotebookPath
}
