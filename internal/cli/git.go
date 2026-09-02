package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/srjn45/warden/internal/lifecycle"
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
			"of the git status/add/commit/rev-parse round-trips.\n\n" +
			"Pass -m to author the message (best — you made the change). Omit it and warden\n" +
			"writes one: the local model from the staged diff if configured, otherwise a\n" +
			"deterministic conventional-commit message from the changed paths.",
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
	cmd.Flags().StringVarP(&message, "message", "m", "", "commit message; if omitted, warden generates one from the diff")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the raw result as JSON")
	return cmd
}

func newPushCmd() *cobra.Command {
	var asJSON bool
	var force bool
	cmd := &cobra.Command{
		Use:   "push",
		Short: "Push the current branch to origin (warden rails + bookkeeping)",
		Long: "Push the current worktree branch to origin, setting upstream.\n\n" +
			"warden refuses to push protected branches (main/master) directly — push your\n" +
			"agent branch and open a PR.\n\n" +
			"Pass --force-with-lease after a rebase or amend to overwrite your remote\n" +
			"branch. warden only ever uses --force-with-lease (never a bare --force), so\n" +
			"the push aborts if a teammate pushed to your branch since your last fetch.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, session := gitTarget()
			res, err := clientFor(cmd).GitPush(context.Background(), session, dir, force)
			if err != nil {
				return err
			}
			if asJSON {
				return emitJSON(cmd, res)
			}
			if res.Forced {
				fmt.Fprintf(cmd.OutOrStdout(), "force-pushed (--force-with-lease) %s -> %s\n", res.Branch, res.Remote)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "pushed %s -> %s\n", res.Branch, res.Remote)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force-with-lease", false, "push with --force-with-lease (safe force after a rebase/amend)")
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

func newCheckRunCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "run [name]",
		Short: "Run the project's configured checks and report only failures",
		Long: "Run the check command(s) declared in this project's .warden/check.yml and\n" +
			"return a pass/fail summary — with captured output for the FAILING checks only,\n" +
			"in place of the hundreds of lines a raw test run spills into the transcript.\n\n" +
			"`wd check run` runs every configured check; `wd check run <name>` runs one (e.g. test,\n" +
			"lint, build). Commands come from the project, so warden stays language-agnostic;\n" +
			"a repo with no .warden/check.yml has nothing to run. Exits non-zero on failure.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var name string
			if len(args) == 1 {
				name = args[0]
			}
			dir, session := gitTarget()
			res, err := clientFor(cmd).Check(context.Background(), session, dir, name)
			if err != nil {
				return err
			}
			if asJSON {
				return emitJSON(cmd, res)
			}
			return printCheckResult(cmd, res)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the raw result as JSON")
	return cmd
}

// printCheckResult renders the per-check pass/fail lines (with failing output)
// and returns a concise error when any check failed, so `wd check` exits non-zero
// for scripts and CI without re-printing the already-shown detail.
func printCheckResult(cmd *cobra.Command, res lifecycle.CheckResult) error {
	out := cmd.OutOrStdout()
	failed := 0
	for _, c := range res.Checks {
		if c.Passed {
			fmt.Fprintf(out, "✓ %s (%s)\n", c.Name, c.Cmd)
			continue
		}
		failed++
		fmt.Fprintf(out, "✗ %s (%s) — exit %d\n%s\n", c.Name, c.Cmd, c.ExitCode, c.Output)
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d check(s) failed", failed, len(res.Checks))
	}
	return nil
}
