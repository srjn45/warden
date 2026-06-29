package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/srjn45/warden/internal/client"
)

// newForkCmd is the ergonomic shorthand for `warden start --fork-from <agent>`: it
// forks an existing agent's recorded session into a NEW warden-managed agent. A fork
// branches the source's conversation/reasoning (codex's session rollout) into a
// divergent session and continues it as its own managed agent — a fresh sibling
// worktree off the source's branch, carrying the source's uncommitted tracked
// changes, with its own tmux session warden monitors and tears down. The source
// agent keeps running, untouched.
//
// It is a THIN wrapper over the one `fork_from` spawn field (no new daemon endpoint),
// mirroring how `warden handoff` wraps spawn. The trailing optional prompt rides the
// existing spawn prompt seam (a divergent first message); omit it to just continue
// the source's conversation. The fork inherits the source's repo and backend
// (resolved daemon-side), so neither is restated here.
func newForkCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "fork <agent> [\"<prompt>\"]",
		Short: "Fork an agent's session into a new managed agent (branches the conversation; the source keeps running)",
		Long: `Fork an existing agent's recorded session into a NEW warden-managed agent.

A fork branches the source agent's conversation/reasoning (codex's session rollout)
into a divergent session and continues it as its own managed agent: a fresh sibling
worktree off the source's branch HEAD, seeded with the source's uncommitted tracked
changes (dirty-tree carry), with its own tmux session warden monitors and tears
down. The source agent keeps running, untouched — fork branches sideways, unlike
snapshot (rewinds one timeline) or rotate/handoff (carry the task, drop the
conversation).

This is the shorthand for ` + "`warden start --fork-from <agent>`" + ` — a managed spawn
whose launch command is the backend's fork verb. Only backends with a native session
fork are forkable (codex today); forking one without (e.g. claude) reports a clean
"cannot fork". The source's backend session id must already be pinned — if it has not
run a turn yet the fork reports that, and you retry once it has.

NOTE: ` + "`git stash create`" + ` carries only TRACKED changes; the source's untracked /
.gitignore'd build artifacts are not seeded into the fork.

  warden fork agent-7                  fork agent-7, continue its conversation
  warden fork agent-7 "now try X"      fork and seed a divergent first prompt`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			source := args[0]
			prompt := ""
			if len(args) == 2 {
				prompt = args[1]
			}
			typ, _ := cmd.Flags().GetString("type")
			name, _ := cmd.Flags().GetString("name")
			model, _ := cmd.Flags().GetString("model")
			permissionMode, _ := cmd.Flags().GetString("permission-mode")
			force, _ := cmd.Flags().GetBool("force")

			s, err := clientFor(cmd).Spawn(cmd.Context(), client.SpawnParams{
				Type: typ, ForkFrom: source, Prompt: prompt, Name: name, Model: model,
				PermissionMode: permissionMode, Force: force,
			})
			if err != nil {
				var cre *client.ErrConfirmationRequired
				if errors.As(err, &cre) {
					fmt.Fprintf(cmd.ErrOrStderr(),
						"⚠ memory pressure: %s\n  re-run with --force to fork anyway\n", cre.Verdict.Reason)
					return fmt.Errorf("fork blocked by memory-pressure gate")
				}
				return err
			}
			nameLabel := ""
			if s.Name != "" {
				nameLabel = fmt.Sprintf(" (%s)", s.Name)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "forked %s → %s%s [%s] (%s) — attach with `warden attach %s`\n",
				source, s.ID, nameLabel, s.Type, s.Status, s.ID)
			return nil
		},
	}
	cmd.Flags().String("type", "development", "worktree-backed task type for the fork (must isolate in its own worktree)")
	cmd.Flags().String("name", "", "optional human-friendly name for the fork")
	cmd.Flags().String("model", "", "model override for the fork (default: the source/backend default)")
	cmd.Flags().String("permission-mode", "", "permission mode for the fork: acceptEdits|auto|bypassPermissions|default|dontAsk|plan (default: from config)")
	cmd.Flags().Bool("force", false, "fork even when the memory-pressure gate warns")
	return cmd
}
