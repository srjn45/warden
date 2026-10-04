package cli

import (
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/srjn45/warden/internal/workspace"
)

// newCleanCmd builds `warden workspace clean` (also the top-level alias
// `warden clean`). It is a thin layer over workspace.Detect / workspace.Apply.
func newCleanCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "clean",
		Short: "Delete merged local/origin branches and prune stale worktrees (previews, then asks)",
		Long: "Find branches and worktrees whose work already landed on the base branch —\n" +
			"including squash merges (detected via merged PRs from `gh`, `git cherry`, or a\n" +
			"trial merge) — show a preview, and after confirmation remove the stale\n" +
			"worktrees, delete the merged local branches, and delete the merged origin\n" +
			"branches in one coordinated operation (in that order).\n\n" +
			"main, master, develop, release/*, the base branch and the primary worktree's\n" +
			"branch are never deleted. Dirty worktrees are kept unless --force. Detection\n" +
			"runs `git fetch --prune origin` first (use --local-only to skip remote work).\n\n" +
			"Different from `wd workspace prune`, which reclaims ORPHANED warden worktrees\n" +
			"under .worktrees (agent record gone); clean targets MERGED branches and the\n" +
			"stale worktrees checked out on them.\n\n" +
			"Available as `wd workspace clean` and the top-level alias `wd clean`.",
		Args: cobra.NoArgs,
		RunE: runClean,
	}
	cmd.Flags().String("repo", "", "repo path (default: current directory)")
	cmd.Flags().Bool("dry-run", false, "show what would be cleaned; change nothing")
	cmd.Flags().Bool("local-only", false, "skip remote (origin) branches and the fetch")
	cmd.Flags().Bool("force", false, "also remove dirty worktrees (does not bypass protected branches)")
	cmd.Flags().Bool("yes", false, "skip the confirmation prompt")
	cmd.Flags().Bool("json", false, "output as JSON (report for --dry-run, result otherwise)")
	return cmd
}

func runClean(cmd *cobra.Command, _ []string) error {
	repo, err := repoFlag(cmd)
	if err != nil {
		return err
	}
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	localOnly, _ := cmd.Flags().GetBool("local-only")
	force, _ := cmd.Flags().GetBool("force")
	yes, _ := cmd.Flags().GetBool("yes")
	jsonOut, _ := cmd.Flags().GetBool("json")
	out := cmd.OutOrStdout()

	opts := workspace.Options{Repo: repo, Fetch: !localOnly, LocalOnly: localOnly}
	rep, err := workspace.Detect(cmd.Context(), opts)
	if err != nil {
		return err
	}
	apply := workspace.ApplyOptions{DryRun: dryRun, Force: force, LocalOnly: localOnly}

	if jsonOut && dryRun {
		return printJSON(out, rep)
	}
	empty := len(rep.LocalBranches)+len(rep.RemoteBranches)+len(rep.StaleWorktrees) == 0
	if !jsonOut {
		if empty {
			fmt.Fprintln(out, "nothing to clean")
			printCleanSkipped(out, rep.Skipped)
			return nil
		}
		printCleanReport(out, rep)
		if dryRun {
			fmt.Fprintf(out, "\nWould reclaim %d worktree(s), delete %d local branch(es), and delete %d remote branch(es). Re-run without --dry-run to apply.\n",
				len(rep.StaleWorktrees), len(rep.LocalBranches), len(rep.RemoteBranches))
			return nil
		}
		if !yes {
			fmt.Fprintf(out, "\nReclaim %d worktrees, delete %d local branches, and delete %d remote branches? [y/N]: ",
				len(rep.StaleWorktrees), len(rep.LocalBranches), len(rep.RemoteBranches))
			var ans string
			_, _ = fmt.Fscanln(cmd.InOrStdin(), &ans)
			if ans != "y" && ans != "Y" {
				fmt.Fprintln(out, "aborted")
				return nil
			}
		}
	} else if empty {
		return printJSON(out, &workspace.ApplyResult{})
	}

	res, err := workspace.Apply(cmd.Context(), opts, apply, rep)
	if err != nil {
		return err
	}
	if jsonOut {
		return printJSON(out, res)
	}
	fmt.Fprintf(out, "\nRemoved %d worktree(s), deleted %d local and %d remote branch(es).\n",
		len(res.WorktreesRemoved), len(res.LocalsDeleted), len(res.RemotesDeleted))
	printCleanSkipped(out, res.Skipped)
	for _, e := range res.Errors {
		fmt.Fprintln(out, "error:", e)
	}
	if len(res.Errors) > 0 {
		return fmt.Errorf("%d item(s) failed", len(res.Errors))
	}
	return nil
}

func printCleanReport(w io.Writer, rep *workspace.Report) {
	branchTable := func(title string, bs []workspace.Branch) {
		if len(bs) == 0 {
			return
		}
		fmt.Fprintf(w, "%s\n", title)
		tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "BRANCH\tKIND\tREASON")
		for _, b := range bs {
			fmt.Fprintf(tw, "%s\t%s\t%s\n", b.Name, b.Kind, b.Reason)
		}
		_ = tw.Flush()
		fmt.Fprintln(w)
	}
	fmt.Fprintf(w, "Base: %s\n\n", rep.Base)
	branchTable("Merged Local Branches", rep.LocalBranches)
	branchTable("Merged Remote Branches", rep.RemoteBranches)
	if len(rep.StaleWorktrees) > 0 {
		fmt.Fprintln(w, "Stale Worktrees")
		tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "PATH\tBRANCH\tDIRTY\tKIND")
		for _, wt := range rep.StaleWorktrees {
			dirty := "no"
			if wt.Dirty {
				dirty = "YES"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", wt.Path, wt.Branch, dirty, wt.Kind)
		}
		_ = tw.Flush()
		fmt.Fprintln(w)
	}
	printCleanSkipped(w, rep.Skipped)
}

func printCleanSkipped(w io.Writer, skipped []workspace.Skip) {
	if len(skipped) == 0 {
		return
	}
	fmt.Fprintln(w, "Skipped")
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	for _, s := range skipped {
		fmt.Fprintf(tw, "%s\t%s\n", s.Name, s.Reason)
	}
	_ = tw.Flush()
}
