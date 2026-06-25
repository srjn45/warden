package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/srjn45/warden/internal/client"
	"github.com/srjn45/warden/internal/snapshot"
)

// snapshotClient is the minimal daemon surface the snapshot verbs need. The
// compile-time assertion below pins it to *client.Client (the rotate.go trick),
// and the narrow interface keeps the orchestration helpers fakeable in tests.
type snapshotClient interface {
	SnapshotCreate(ctx context.Context, session, dir, message string) (*snapshot.Snapshot, error)
	SnapshotList(ctx context.Context, session string) ([]*snapshot.Snapshot, error)
	SnapshotRestore(ctx context.Context, id string, force bool) (*snapshot.RestoreResult, error)
}

// Client must satisfy snapshotClient.
var _ snapshotClient = (*client.Client)(nil)

// snapshotTarget resolves the session + dir for a create/list scoped to "the
// current agent": an explicit name arg wins (snapshot another agent by id), else
// the WARDEN_SESSION_ID of the agent this command runs inside. dir is always the
// cwd — the daemon pins to the resolved agent's own worktree when the session is
// known, so a stale cwd cannot misdirect the capture.
func snapshotTarget(name string) (session, dir string) {
	session = name
	if session == "" {
		session = envID("SESSION_ID")
	}
	if wd, err := os.Getwd(); err == nil {
		dir = wd
	}
	return session, dir
}

func newSnapshotCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "snapshot",
		Short: "Checkpoint an agent's worktree + transcript, list checkpoints, and restore one",
		Long: "Capture a known-good point for an agent — its worktree state (a non-destructive\n" +
			"git stash, so the agent's tree is untouched) plus its session transcript — and\n" +
			"roll back to it later. Subcommands: create, list, restore.",
	}
	cmd.AddCommand(newSnapshotCreateCmd(), newSnapshotListCmd(), newSnapshotRestoreCmd())
	return cmd
}

func newSnapshotCreateCmd() *cobra.Command {
	var message string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "create [name]",
		Short: "Capture the worktree state + transcript of an agent (defaults to the current one)",
		Long: "Capture a snapshot of an agent's worktree and session transcript. With no\n" +
			"[name] it snapshots the agent this command runs inside (WARDEN_SESSION_ID);\n" +
			"pass an agent id to snapshot a different one. The worktree is captured with\n" +
			"`git stash create` — non-destructive, so the agent's working tree is untouched.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			session, dir := snapshotTarget(firstArg(args))
			snap, err := clientFor(cmd).SnapshotCreate(context.Background(), session, dir, message)
			if err != nil {
				return err
			}
			if asJSON {
				return emitJSON(cmd, snap)
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "snapshot %s captured on %s @ %s\n", snap.ID, snap.Branch, shortSHA(snap.HeadSHA))
			if len(snap.DirtyFiles) > 0 {
				fmt.Fprintf(out, "  %d uncommitted file(s) stashed (stash %s)\n", len(snap.DirtyFiles), shortSHA(snap.StashSHA))
			} else {
				fmt.Fprintln(out, "  clean tree (no uncommitted changes to stash)")
			}
			if snap.TranscriptPath != "" {
				fmt.Fprintf(out, "  transcript: %s (%d lines)\n", snap.TranscriptPath, snap.TranscriptLines)
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&message, "message", "m", "", "optional label for the snapshot")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the raw result as JSON")
	return cmd
}

func newSnapshotListCmd() *cobra.Command {
	var asJSON bool
	var all bool
	cmd := &cobra.Command{
		Use:   "list [name]",
		Short: "List snapshots for an agent (newest first); --all for every session",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			session, _ := snapshotTarget(firstArg(args))
			if all {
				session = ""
			}
			snaps, err := clientFor(cmd).SnapshotList(context.Background(), session)
			if err != nil {
				return err
			}
			if asJSON {
				return emitJSON(cmd, snaps)
			}
			out := cmd.OutOrStdout()
			if len(snaps) == 0 {
				fmt.Fprintln(out, "no snapshots")
				return nil
			}
			for _, s := range snaps {
				label := ""
				if s.Message != "" {
					label = "  " + s.Message
				}
				fmt.Fprintf(out, "%s  %s  %s @ %s  (%d dirty)%s\n",
					s.ID, s.CreatedAt.Format("2006-01-02 15:04"), s.Branch, shortSHA(s.HeadSHA), len(s.DirtyFiles), label)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the raw result as JSON")
	cmd.Flags().BoolVar(&all, "all", false, "list snapshots across all sessions")
	return cmd
}

func newSnapshotRestoreCmd() *cobra.Command {
	var force bool
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "restore <snapshot-id>",
		Short: "Re-apply a snapshot onto its recorded worktree (refuses a dirty tree unless --force)",
		Long: "Re-apply a snapshot's captured worktree state onto the worktree it was taken\n" +
			"in. Refuses a dirty tree unless --force, and never restores onto main/master.\n" +
			"Reversible-safe: it re-applies the stash without resetting HEAD or dropping the\n" +
			"snapshot, so the snapshot stays usable and uncommitted work is the only thing at\n" +
			"risk (hence the dirty-tree guard).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := clientFor(cmd).SnapshotRestore(context.Background(), args[0], force)
			if err != nil {
				return err
			}
			if asJSON {
				return emitJSON(cmd, res)
			}
			return printSnapshotRestore(cmd, res)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "restore even when the worktree has uncommitted changes")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the raw result as JSON")
	return cmd
}

// printSnapshotRestore renders the human summary of a restore, surfacing the
// HEAD-drift warning and any conflicting paths for the operator to resolve.
func printSnapshotRestore(cmd *cobra.Command, res *snapshot.RestoreResult) error {
	out := cmd.OutOrStdout()
	if len(res.Conflicts) > 0 {
		fmt.Fprintf(out, "restored %s with conflicts — resolve these files:\n", res.SnapshotID)
		for _, f := range res.Conflicts {
			fmt.Fprintf(out, "  %s\n", f)
		}
	} else if res.Applied {
		fmt.Fprintf(out, "restored %s onto %s (re-applied captured changes)\n", res.SnapshotID, res.Branch)
	} else {
		fmt.Fprintf(out, "restored %s onto %s (clean capture — no changes to re-apply)\n", res.SnapshotID, res.Branch)
	}
	if !res.HeadMatch {
		fmt.Fprintf(out, "  WARNING: HEAD moved since capture (snapshot %s, now %s) — review the result\n",
			shortSHA(res.SnapshotHead), shortSHA(res.CurrentHead))
	}
	if res.TranscriptPath != "" {
		fmt.Fprintf(out, "  transcript: %s\n", res.TranscriptPath)
	}
	return nil
}

// firstArg returns args[0] or "" — the optional [name] for create/list.
func firstArg(args []string) string {
	if len(args) > 0 {
		return args[0]
	}
	return ""
}

// shortSHA truncates a git SHA to 8 chars for display (leaves short/empty as-is).
func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
