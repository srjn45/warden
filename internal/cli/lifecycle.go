package cli

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"
	"github.com/srajanpathak/agentctl/internal/client"
)

func newStartCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "start [TICKET|\"<prompt>\"] [--type <TYPE>]",
		Short: "Spawn an agent — `start \"<prompt>\"` (auto-typed) or `start TICKET --type <TYPE>` (managed worktree)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			typ, _ := cmd.Flags().GetString("type")

			// Prompt mode: `agentctl start "<prompt>"` with no --type.
			if typ == "" {
				if len(args) != 1 {
					return fmt.Errorf("provide a prompt: agentctl start \"<prompt>\"  (or use --type for a managed worktree)")
				}
				s, err := clientFor(cmd).Spawn(cmd.Context(), client.SpawnParams{Prompt: args[0]})
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "spawned %s (classifying…) — attach with `agentctl attach %s`\n", s.ID, s.ID)
				return nil
			}

			// Typed/managed worktree mode (unchanged).
			repo, _ := cmd.Flags().GetString("repo")
			if repo == "" {
				cwd, err := os.Getwd()
				if err != nil {
					return err
				}
				repo = cwd
			}
			branch, _ := cmd.Flags().GetString("branch")
			pr, _ := cmd.Flags().GetString("pr")
			worktree, _ := cmd.Flags().GetBool("worktree")
			if typ == "pr-review" && pr == "" && branch == "" {
				return fmt.Errorf("pr-review needs --pr or --branch")
			}
			ticket := ""
			if len(args) == 1 {
				ticket = args[0]
			}
			s, err := clientFor(cmd).Spawn(cmd.Context(), client.SpawnParams{
				Type: typ, Ticket: ticket, Repo: repo, Branch: branch, PR: pr, Worktree: worktree,
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "spawned %s [%s] (%s) — attach with `agentctl attach %s`\n", s.ID, s.Type, s.Status, s.ID)
			return nil
		},
	}
	cmd.Flags().String("type", "", "task type: development|analysis|spike|pr-review|buildkite-debug|test-run|env-test|other")
	cmd.Flags().String("repo", "", "repo path (default: current directory)")
	cmd.Flags().String("branch", "", "new branch (development) or checkout target (pr-review)")
	cmd.Flags().String("pr", "", "PR number/url (pr-review)")
	cmd.Flags().Bool("worktree", false, "create a scratch worktree for analysis/spike")
	return cmd
}

func newDoneCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "done <TICKET>",
		Short: "Tear down an agent (kill tmux, prune worktree/branch, archive doc)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			force, _ := cmd.Flags().GetBool("force")
			hard, _ := cmd.Flags().GetBool("hard")
			if err := clientFor(cmd).Cleanup(cmd.Context(), args[0], force, hard); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "cleaned up %s\n", args[0])
			return nil
		},
	}
	cmd.Flags().Bool("force", false, "override the uncommitted/unpushed guard")
	cmd.Flags().Bool("hard", false, "hard-delete the doc instead of archiving")
	return cmd
}

func newAttachCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "attach <TICKET>",
		Short: "Attach to the agent's tmux session",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Replaces the current process with an interactive tmux attach.
			tmux, err := exec.LookPath("tmux")
			if err != nil {
				return err
			}
			c := exec.Command(tmux, "attach", "-t", args[0])
			c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
			return c.Run()
		},
	}
}
