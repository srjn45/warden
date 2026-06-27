package cli

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/srjn45/warden/internal/client"
	"github.com/srjn45/warden/internal/lifecycle"
)

// repoFlag resolves the --repo flag, defaulting to the current directory (the
// same convention `start --type` uses for its managed worktree).
func repoFlag(cmd *cobra.Command) (string, error) {
	repo, _ := cmd.Flags().GetString("repo")
	if repo != "" {
		return repo, nil
	}
	return os.Getwd()
}

// newWorktreeCmd is the unified "worktree" umbrella over warden's two worktree
// operations: LIST (inspect the warden-owned worktrees under .worktrees, joined
// to active/archived records) and PRUNE (reclaim orphaned worktrees). It adds no
// storage or logic of its own — it is a single discoverable entry point that
// reuses the existing list and prune run functions. `wd worktree` with no
// subcommand prints the list (the same view as `wd worktree list`), kept for
// back-compat. `wd worktree prune` is the very same command as the top-level
// `wd prune`, which remains available and unchanged as an alias.
func newWorktreeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "worktree",
		Short: "Inspect and reclaim warden's git worktrees: list and prune, in one place",
		Long: "One umbrella over warden's two worktree operations:\n" +
			"  • LIST  — the warden-owned worktrees under .worktrees, joined to active/archived records (`wd worktree list`)\n" +
			"  • PRUNE — reclaim orphaned warden worktrees (`wd worktree prune`)\n\n" +
			"`wd worktree` with no subcommand prints the list (the same view as `wd worktree list`). " +
			"`wd worktree prune` is the same command as the top-level `wd prune`, which remains available " +
			"and unchanged as an alias. (Tearing down ONE agent's worktree is a different concern — see " +
			"`wd remove-worktree`.)",
		Args: cobra.NoArgs,
		RunE: runWorktreeList,
	}
	addWorktreeListFlags(cmd)
	cmd.AddCommand(newWorktreeListCmd(), newPruneCmd())
	return cmd
}

func newWorktreeListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List warden worktrees under .worktrees, joined to active/archived records",
		Args:    cobra.NoArgs,
		RunE:    runWorktreeList,
	}
	addWorktreeListFlags(cmd)
	return cmd
}

// addWorktreeListFlags installs the flags shared by the bare `wd worktree`
// (back-compat list) and the explicit `wd worktree list` subcommand.
func addWorktreeListFlags(cmd *cobra.Command) {
	cmd.Flags().String("repo", "", "repo path (default: current directory)")
	cmd.Flags().Bool("json", false, "output as JSON")
}

// runWorktreeList renders the warden-owned worktrees under .worktrees, joined to
// their active/archived records. It backs both the bare `wd worktree` and the
// explicit `wd worktree list`, so the two never diverge.
func runWorktreeList(cmd *cobra.Command, _ []string) error {
	repo, err := repoFlag(cmd)
	if err != nil {
		return err
	}
	rows, err := clientFor(cmd).ListWorktrees(cmd.Context(), repo)
	if err != nil {
		return err
	}
	if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
		if rows == nil {
			rows = []lifecycle.WorktreeListing{}
		}
		return printJSON(cmd.OutOrStdout(), rows)
	}
	if len(rows) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "no warden worktrees under .worktrees")
		return nil
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "PATH\tBRANCH\tOWNER\tSTATE")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.Path, branchCell(r), ownerCell(r.Owner, r.Lifecycle), r.State)
	}
	return tw.Flush()
}

func branchCell(r lifecycle.WorktreeListing) string {
	if r.Branch == "" {
		return "(detached)"
	}
	return r.Branch
}

func ownerCell(owner, life string) string {
	if owner == "" {
		return "orphan"
	}
	return fmt.Sprintf("%s (%s)", owner, life)
}

// newPruneCmd backs both `wd worktree prune` (the subcommand under the worktree
// umbrella) and the top-level `wd prune` alias — the same constructor is wired
// into both, so flags, prompts, and output are identical. It reclaims orphaned
// warden worktrees under .worktrees; retention is policy-driven via the
// `worktree.keep_done` / `worktree.auto_prune` config settings.
func newPruneCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Reclaim orphaned warden worktrees under .worktrees (always asks; --force overrides guards)",
		Long: "Reclaim orphaned warden worktrees under .worktrees — those whose owning agent\n" +
			"record is gone (or, with --include-archived, archived). Always shows the plan\n" +
			"and asks before removing anything (skip with --yes); dirty/unpushed worktrees\n" +
			"are kept unless --force. Retention is policy-driven via the `worktree.keep_done`\n" +
			"/ `worktree.auto_prune` config settings.\n\n" +
			"Available as `wd worktree prune` and, unchanged, as the top-level alias `wd prune`.\n" +
			"(To tear down ONE agent's worktree instead, use `wd remove-worktree`.)",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, err := repoFlag(cmd)
			if err != nil {
				return err
			}
			dryRun, _ := cmd.Flags().GetBool("dry-run")
			force, _ := cmd.Flags().GetBool("force")
			includeArchived, _ := cmd.Flags().GetBool("include-archived")
			yes, _ := cmd.Flags().GetBool("yes")
			c := clientFor(cmd)

			// A non-dry-run removal prompts unless --yes, mirroring remove-worktree.
			// Show the plan (a dry-run) first so the prompt is informed.
			if !dryRun && !yes {
				plan, err := c.Prune(cmd.Context(), client.PruneParams{Repo: repo, DryRun: true, Force: force, IncludeArchived: includeArchived})
				if err != nil {
					return err
				}
				printPrune(cmd, plan, true)
				removable := countAction(plan, lifecycle.PruneRemove)
				if removable == 0 {
					return nil
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Remove %d worktree(s)? This cannot be undone. [y/N]: ", removable)
				var ans string
				_, _ = fmt.Fscanln(cmd.InOrStdin(), &ans)
				if ans != "y" && ans != "Y" {
					fmt.Fprintln(cmd.OutOrStdout(), "aborted")
					return nil
				}
			}

			results, err := c.Prune(cmd.Context(), client.PruneParams{Repo: repo, DryRun: dryRun, Force: force, IncludeArchived: includeArchived})
			if err != nil {
				return err
			}
			if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
				if results == nil {
					results = []lifecycle.PruneResult{}
				}
				return printJSON(cmd.OutOrStdout(), results)
			}
			printPrune(cmd, results, dryRun)
			return nil
		},
	}
	cmd.Flags().String("repo", "", "repo path (default: current directory)")
	cmd.Flags().Bool("dry-run", false, "report what would be removed; change nothing")
	cmd.Flags().Bool("force", false, "override the dirty/unpushed guard and permit branch deletion for record-less orphans (never the default branch)")
	cmd.Flags().Bool("include-archived", false, "also reclaim worktrees owned by archived (done) records")
	cmd.Flags().Bool("yes", false, "skip the confirmation prompt")
	cmd.Flags().Bool("json", false, "output as JSON")
	return cmd
}

func countAction(results []lifecycle.PruneResult, action lifecycle.PruneAction) int {
	n := 0
	for _, r := range results {
		if r.Action == action {
			n++
		}
	}
	return n
}

func printPrune(cmd *cobra.Command, results []lifecycle.PruneResult, dryRun bool) {
	if len(results) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "no warden worktrees under .worktrees")
		return
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "ACTION\tPATH\tBRANCH\tOWNER\tSTATE\tDETAIL")
	for _, r := range results {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			pruneActionCell(r.Action, dryRun), r.Path, branchCell(lifecycle.WorktreeListing{Branch: r.Branch}),
			ownerCell(r.Owner, r.Lifecycle), r.State, pruneDetail(r))
	}
	_ = tw.Flush()

	removable := countAction(results, lifecycle.PruneRemove)
	skipped := countAction(results, lifecycle.PruneSkip)
	kept := countAction(results, lifecycle.PruneKeep)
	branches := 0
	for _, r := range results {
		if r.Action == lifecycle.PruneRemove && r.BranchDeleted {
			branches++
		}
	}
	if dryRun {
		fmt.Fprintf(cmd.OutOrStdout(), "\nSummary: %d removable, %d blocked, %d kept. Re-run without --dry-run to apply.\n", removable, skipped, kept)
		return
	}
	fmt.Fprintf(cmd.OutOrStdout(), "\nRemoved %d worktree(s), reclaimed %d branch(es); %d skipped.\n", removable, branches, skipped)
}

func pruneActionCell(a lifecycle.PruneAction, dryRun bool) string {
	switch a {
	case lifecycle.PruneRemove:
		if dryRun {
			return "would remove"
		}
		return "removed"
	case lifecycle.PruneSkip:
		if dryRun {
			return "SKIP"
		}
		return "SKIPPED"
	default:
		return "keep"
	}
}

func pruneDetail(r lifecycle.PruneResult) string {
	if r.Action == lifecycle.PruneRemove && r.BranchDeleted {
		if r.Reason == "" {
			return "+ branch"
		}
		return r.Reason + "; + branch"
	}
	return r.Reason
}
