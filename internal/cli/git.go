package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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

// Short errors returned after the detail has already been printed, so the CLI
// exits non-zero (HTTP/MCP results are unchanged: they still return the result).
var (
	errCommitRejected = errors.New("commit rejected by a pre-commit hook")
	errSyncConflicts  = errors.New("sync stopped on conflicts")
)

func newCommitCmd() *cobra.Command {
	var message string
	var asJSON, amend, force bool
	cmd := &cobra.Command{
		Use:   "commit [paths...]",
		Short: "Stage and commit the worktree (warden rails + hooks + bookkeeping)",
		Long: "Stage and commit every change in the current worktree on its branch.\n\n" +
			"warden refuses protected branches (main/master), runs pre-commit hooks and\n" +
			"returns only failures, and links the commit to this agent — one call in place\n" +
			"of the git status/add/commit/rev-parse round-trips.\n\n" +
			"Pass -m to author the message (best — you made the change). Omit it and warden\n" +
			"writes one: the local model from the staged diff if configured, otherwise a\n" +
			"deterministic conventional-commit message from the changed paths.\n\n" +
			"Give paths (relative to the current directory) to stage and commit only those;\n" +
			"paths outside the repository are rejected. With no paths everything is staged.\n\n" +
			"--amend rewrites the last commit, keeping its message unless -m is given. It is\n" +
			"refused on a merge commit and on a commit already in the upstream branch unless\n" +
			"--force (after which push needs --force-with-lease).",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			dir, session := gitTarget()
			paths := make([]string, 0, len(args))
			for _, a := range args {
				if !filepath.IsAbs(a) {
					a = filepath.Join(dir, a)
				}
				paths = append(paths, a)
			}
			if force && !amend {
				return errors.New("--force only applies with --amend")
			}
			res, err := clientFor(cmd).GitCommitWith(context.Background(), session, dir, lifecycle.CommitOptions{Message: message, Paths: paths, Amend: amend, Force: force})
			if err != nil {
				return err
			}
			if res.Warning != "" && !asJSON {
				fmt.Fprintln(cmd.ErrOrStderr(), "warning: "+res.Warning)
			}
			if asJSON {
				if err := emitJSON(cmd, res); err != nil {
					return err
				}
				if res.HookFailed {
					return errCommitRejected
				}
				return nil
			}
			switch {
			case res.HookFailed:
				fmt.Fprintf(cmd.OutOrStdout(), "commit rejected by a pre-commit hook:\n%s\n", res.HookOutput)
				return errCommitRejected
			case !res.Committed:
				fmt.Fprintln(cmd.OutOrStdout(), "nothing to commit (clean tree)")
			default:
				fmt.Fprintf(cmd.OutOrStdout(), "committed %s on %s (%d file(s))\n", res.SHA, res.Branch, len(res.Files))
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&message, "message", "m", "", "commit message; if omitted, warden generates one from the diff")
	cmd.Flags().BoolVar(&amend, "amend", false, "rewrite the last commit (keeps its message unless -m is given)")
	cmd.Flags().BoolVar(&force, "force", false, "with --amend, allow amending a commit already in the upstream branch")
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
			if res.UpToDate {
				fmt.Fprintf(cmd.OutOrStdout(), "already up to date: %s -> %s\n", res.Branch, res.Remote)
			} else if res.Forced {
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
	var asJSON, cont, abort bool
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Fetch and rebase the current branch onto its base (warden conflict detect)",
		Long: "Fetch origin and rebase the current branch onto origin/<base> (default main).\n\n" +
			"Refuses a dirty tree (commit first). On conflict warden leaves the rebase in\n" +
			"progress and reports only the conflicting files for you to resolve.\n\n" +
			"While a rebase is in progress, `wd sync --continue` stages your resolved files and\n" +
			"finishes it (reporting any conflicts from the next commit), and `wd sync --abort`\n" +
			"drops it and restores the branch. A plain sync or `wd commit` is refused until then.\n\n" +
			"Exits non-zero when the rebase stops on conflicts.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if cont && abort {
				return fmt.Errorf("--continue and --abort are mutually exclusive")
			}
			if (cont || abort) && base != "" {
				return fmt.Errorf("--base cannot be combined with --continue or --abort")
			}
			dir, session := gitTarget()
			var res lifecycle.SyncResult
			var err error
			switch {
			case cont:
				res, err = clientFor(cmd).GitSyncContinue(context.Background(), session, dir)
			case abort:
				res, err = clientFor(cmd).GitSyncAbort(context.Background(), session, dir)
			default:
				res, err = clientFor(cmd).GitSync(context.Background(), session, dir, base)
			}
			if err != nil {
				return err
			}
			if asJSON {
				if err := emitJSON(cmd, res); err != nil {
					return err
				}
				if len(res.Conflicts) > 0 {
					return errSyncConflicts
				}
				return nil
			}
			if len(res.Conflicts) > 0 {
				fmt.Fprintf(cmd.OutOrStdout(),
					"rebase hit conflicts — resolve these files, then `wd sync --continue` (`git rebase --continue` also works), or `wd sync --abort`:\n  %s\n",
					strings.Join(res.Conflicts, "\n  "))
				return errSyncConflicts
			}
			if abort {
				fmt.Fprintf(cmd.OutOrStdout(), "rebase aborted; %s restored\n", res.Branch)
				return nil
			}
			if cont {
				fmt.Fprintf(cmd.OutOrStdout(), "rebase continued; %s is up to date\n", res.Branch)
				return nil
			}
			why := ""
			if res.BaseSource != "" {
				why = " (defaulted from " + res.BaseSource + ")"
			}
			if res.UpToDate {
				fmt.Fprintf(cmd.OutOrStdout(), "already up to date with origin/%s\n", res.Base)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "rebased %s onto origin/%s%s\n", res.Branch, res.Base, why)
			return nil
		},
	}
	cmd.Flags().StringVar(&base, "base", "", "base branch to rebase onto (default main)")
	cmd.Flags().BoolVar(&cont, "continue", false, "finish a conflicted rebase: stage resolved files and run git rebase --continue")
	cmd.Flags().BoolVar(&abort, "abort", false, "drop a rebase in progress and restore the branch")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the raw result as JSON")
	return cmd
}

func newPRCmd() *cobra.Command {
	var base, title, body, bodyFile string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "pr [agent-id]",
		Short: "Open (or return the already-open) pull request for the agent's branch",
		Long: "Push the agent's branch and open a GitHub pull request for it, without ending the\n" +
			"agent. Idempotent: when a PR is already open for the branch it is returned instead.\n\n" +
			"The agent comes from WARDEN_SESSION_ID (set in every warden-spawned session) or the\n" +
			"optional [agent-id] argument. Without either there is no agent to resolve a branch\n" +
			"and base from, so use `gh pr create` directly.\n\n" +
			"--base defaults like `wd git sync`: the agent's recorded base, its autopilot\n" +
			"integration branch, then the repository default. --title / --body (or --body-file,\n" +
			"`-` for stdin) are used verbatim; omitted ones are drafted from the agent's work.\n" +
			"main/master are refused as the PR head.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if body != "" && bodyFile != "" {
				return fmt.Errorf("--body and --body-file are mutually exclusive")
			}
			if bodyFile != "" {
				var b []byte
				var err error
				if bodyFile == "-" {
					b, err = io.ReadAll(cmd.InOrStdin())
				} else {
					b, err = os.ReadFile(bodyFile)
				}
				if err != nil {
					return fmt.Errorf("read --body-file: %w", err)
				}
				body = string(b)
			}
			_, session := gitTarget()
			if len(args) == 1 {
				session = args[0]
			}
			if session == "" {
				return errors.New("no agent session: WARDEN_SESSION_ID is unset and no agent id was given — run `gh pr create` directly, or pass an agent id")
			}
			res, err := clientFor(cmd).CreatePRWith(context.Background(), session, base, title, body)
			if err != nil {
				return err
			}
			if asJSON {
				return emitJSON(cmd, res)
			}
			verb := "opened"
			if !res.Created {
				verb = "already open:"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "PR %s %s (%s -> %s)\n", verb, res.URL, res.Branch, res.Base)
			return nil
		},
	}
	cmd.Flags().StringVar(&base, "base", "", "PR base branch (default: the agent's recorded base, resolved as for git sync)")
	cmd.Flags().StringVar(&title, "title", "", "PR title (default: drafted from the agent's work)")
	cmd.Flags().StringVar(&body, "body", "", "PR body (default: drafted from the agent's work)")
	cmd.Flags().StringVar(&bodyFile, "body-file", "", "read the PR body from a file (- for stdin)")
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

func newCheckListCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List configured project checks without running them",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, session := gitTarget()
			checks, err := clientFor(cmd).ListChecks(context.Background(), session, dir)
			if err != nil {
				return err
			}
			if asJSON {
				return emitJSON(cmd, checks)
			}
			for _, check := range checks {
				if check.Dir == "" {
					fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t.\n", check.Name, check.Cmd)
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", check.Name, check.Cmd, check.Dir)
				}
			}
			return nil
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
