package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// gitTarget resolves the working dir and owning session for a wd git command:
// the cwd is the agent's worktree, and WARDEN_SESSION_ID (set in every
// warden-spawned tmux session) ties the action to the agent record. Both degrade
// gracefully — a human running `wd commit` outside an agent gets dir-only.
func gitTarget() (dir, session string) {
	if wd, err := os.Getwd(); err == nil {
		dir = wd
	}
	return dir, envID("SESSION_ID")
}

// emitJSON prints v as indented JSON to the command's stdout.
func emitJSON(cmd *cobra.Command, v any) error {
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func newCommitCmd() *cobra.Command {
	var message string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "commit",
		Short: "Stage and commit the worktree (warden rails + hooks + bookkeeping)",
		Long: "Stage and commit every change in the current worktree on its branch.\n\n" +
			"warden refuses protected branches (main/master), runs pre-commit hooks and\n" +
			"returns only failures, and links the commit to this agent — one call in place\n" +
			"of the git status/add/commit/rev-parse round-trips.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, session := gitTarget()
			res, err := clientFor(cmd).GitCommit(context.Background(), session, dir, message)
			if err != nil {
				return err
			}
			if asJSON {
				return emitJSON(cmd, res)
			}
			switch {
			case res.HookFailed:
				fmt.Fprintf(cmd.OutOrStdout(), "commit rejected by a pre-commit hook:\n%s\n", res.HookOutput)
			case !res.Committed:
				fmt.Fprintln(cmd.OutOrStdout(), "nothing to commit (clean tree)")
			default:
				fmt.Fprintf(cmd.OutOrStdout(), "committed %s on %s (%d file(s))\n", res.SHA, res.Branch, len(res.Files))
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&message, "message", "m", "", "commit message (required)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the raw result as JSON")
	_ = cmd.MarkFlagRequired("message")
	return cmd
}

func newPushCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "push",
		Short: "Push the current branch to origin (warden rails + bookkeeping)",
		Long: "Push the current worktree branch to origin, setting upstream.\n\n" +
			"warden refuses to push protected branches (main/master) directly — push your\n" +
			"agent branch and open a PR.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, session := gitTarget()
			res, err := clientFor(cmd).GitPush(context.Background(), session, dir)
			if err != nil {
				return err
			}
			if asJSON {
				return emitJSON(cmd, res)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "pushed %s -> %s\n", res.Branch, res.Remote)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the raw result as JSON")
	return cmd
}

func newSyncCmd() *cobra.Command {
	var base string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Fetch and rebase the current branch onto its base (warden conflict detect)",
		Long: "Fetch origin and rebase the current branch onto origin/<base> (default main).\n\n" +
			"Refuses a dirty tree (commit first). On conflict warden leaves the rebase in\n" +
			"progress and reports only the conflicting files for you to resolve.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, session := gitTarget()
			res, err := clientFor(cmd).GitSync(context.Background(), session, dir, base)
			if err != nil {
				return err
			}
			if asJSON {
				return emitJSON(cmd, res)
			}
			if len(res.Conflicts) > 0 {
				fmt.Fprintf(cmd.OutOrStdout(),
					"rebase onto origin/%s hit conflicts — resolve these files, then `git rebase --continue`:\n  %s\n",
					res.Base, strings.Join(res.Conflicts, "\n  "))
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "rebased %s onto origin/%s\n", res.Branch, res.Base)
			return nil
		},
	}
	cmd.Flags().StringVar(&base, "base", "", "base branch to rebase onto (default main)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the raw result as JSON")
	return cmd
}
