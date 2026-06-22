package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/srjn45/warden/internal/client"
)

// promptFromArgs returns the prompt for a free-form (no --type) spawn: the
// single positional argument, or "" when none is given — an empty prompt opens
// claude interactively in the launch dir and waits for instructions.
func promptFromArgs(args []string) string {
	if len(args) == 1 {
		return args[0]
	}
	return ""
}

func newStartCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "start [TICKET|\"<prompt>\"] [--type <TYPE>] [--dir <PATH>]",
		Short: "Spawn an agent — `start \"<prompt>\"` (auto-typed), `start --dir <path>` (interactive: open Claude & wait), or `start TICKET --type <TYPE>` (managed worktree)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			typ, _ := cmd.Flags().GetString("type")

			// Free-form mode: `warden start "<prompt>" [--dir]` (autonomous) or
			// `warden start --dir <path>` with no prompt (interactive: opens
			// claude in the dir and waits). No --type.
			if typ == "" {
				prompt := promptFromArgs(args)
				dirFlag, _ := cmd.Flags().GetString("dir")
				dir, err := resolveDir(dirFlag)
				if err != nil {
					return err
				}
				name, _ := cmd.Flags().GetString("name")
				supervised, _ := cmd.Flags().GetBool("supervised")
				permissionMode, _ := cmd.Flags().GetString("permission-mode")
				// --supervised is an alias for --permission-mode acceptEdits
				if supervised && permissionMode == "" {
					permissionMode = "acceptEdits"
				}
				autoRestart, _ := cmd.Flags().GetBool("auto-restart")
				force, _ := cmd.Flags().GetBool("force")
				model, _ := cmd.Flags().GetString("model")
				s, err := clientFor(cmd).Spawn(cmd.Context(), client.SpawnParams{Name: name, Prompt: prompt, Cwd: dir, PermissionMode: permissionMode, AutoRestart: autoRestart, Force: force, Model: model})
				if err != nil {
					var cre *client.ErrConfirmationRequired
					if errors.As(err, &cre) {
						fmt.Fprintf(cmd.ErrOrStderr(),
							"⚠ memory pressure: %s\n  re-run with --force to spawn anyway\n", cre.Verdict.Reason)
						return fmt.Errorf("spawn blocked by memory-pressure gate")
					}
					return err
				}
				nameLabel := ""
				if s.Name != "" {
					nameLabel = fmt.Sprintf(" (%s)", s.Name)
				}
				outcome := fmt.Sprintf("spawned %s%s (classifying…)", s.ID, nameLabel)
				if prompt == "" {
					outcome = fmt.Sprintf("opened interactive agent %s%s", s.ID, nameLabel)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s — attach with `warden attach %s`\n", outcome, s.ID)
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
			name, _ := cmd.Flags().GetString("name")
			branch, _ := cmd.Flags().GetString("branch")
			pr, _ := cmd.Flags().GetString("pr")
			worktree, _ := cmd.Flags().GetBool("worktree")
			supervised, _ := cmd.Flags().GetBool("supervised")
			permissionMode, _ := cmd.Flags().GetString("permission-mode")
			// --supervised is an alias for --permission-mode acceptEdits
			if supervised && permissionMode == "" {
				permissionMode = "acceptEdits"
			}
			autoRestart, _ := cmd.Flags().GetBool("auto-restart")
			if typ == "pr-review" && pr == "" && branch == "" {
				return fmt.Errorf("pr-review needs --pr or --branch")
			}
			ticket := ""
			if len(args) == 1 {
				ticket = args[0]
			}
			force, _ := cmd.Flags().GetBool("force")
			model, _ := cmd.Flags().GetString("model")
			s, err := clientFor(cmd).Spawn(cmd.Context(), client.SpawnParams{
				Name: name, Type: typ, Ticket: ticket, Repo: repo, Branch: branch, PR: pr, Worktree: worktree, PermissionMode: permissionMode, AutoRestart: autoRestart, Force: force, Model: model,
			})
			if err != nil {
				var cre *client.ErrConfirmationRequired
				if errors.As(err, &cre) {
					fmt.Fprintf(cmd.ErrOrStderr(),
						"⚠ memory pressure: %s\n  re-run with --force to spawn anyway\n", cre.Verdict.Reason)
					return fmt.Errorf("spawn blocked by memory-pressure gate")
				}
				return err
			}
			nameLabel := ""
			if s.Name != "" {
				nameLabel = fmt.Sprintf(" (%s)", s.Name)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "spawned %s%s [%s] (%s) — attach with `warden attach %s`\n", s.ID, nameLabel, s.Type, s.Status, s.ID)
			return nil
		},
	}
	cmd.Flags().String("name", "", "optional human-friendly name (max 32 chars, alphanumeric + hyphens/underscores)")
	cmd.Flags().String("type", "", "task type: development|analysis|spike|pr-review|code|docs|website|debug-ci|tests|other")
	cmd.Flags().String("repo", "", "repo path (default: current directory)")
	cmd.Flags().String("branch", "", "new branch (development) or checkout target (pr-review)")
	cmd.Flags().String("pr", "", "PR number/url (pr-review)")
	cmd.Flags().Bool("worktree", false, "create a scratch worktree for analysis/spike")
	cmd.Flags().String("dir", "", "directory to launch the agent from (default: current directory)")
	cmd.Flags().Bool("supervised", false, "alias for --permission-mode acceptEdits (kept for backwards compatibility)")
	cmd.Flags().String("permission-mode", "", "permission mode: acceptEdits|auto|bypassPermissions|default|dontAsk|plan (default: from config or 'auto')")
	cmd.Flags().Bool("auto-restart", false, "auto-resume this agent if it crashes (errored), capped at a few attempts")
	cmd.Flags().Bool("force", false, "spawn even when the memory-pressure gate warns")
	cmd.Flags().String("model", "", "claude model: opus, sonnet, haiku, fable, or full model ID (default: the model_default config setting, i.e. sonnet)")
	return cmd
}

// resolveDir returns the explicit --dir flag value (resolved to an absolute
// path against the caller's cwd), or the current working directory when the
// flag is empty. This is where the agent's claude is launched.
// Resolve to absolute HERE (in the CLI process, where cwd is correct), not in
// the daemon which runs under launchd with a different cwd.
func resolveDir(flagVal string) (string, error) {
	if flagVal != "" {
		// Resolve a relative --dir against the CALLER's cwd (here), not the
		// daemon's: the daemon runs under launchd with a different cwd.
		return filepath.Abs(flagVal)
	}
	return os.Getwd()
}

func newRestoreCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "restore <TICKET>",
		Short: "Recreate and resume a lost/orphaned agent (claude --resume)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := clientFor(cmd).Restore(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "restoring %s\n", args[0])
			return nil
		},
	}
}

func newTerminateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "terminate <TICKET>",
		Short: "Stop an agent: kill its tmux+claude session (keeps the record and worktree)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := clientFor(cmd).Terminate(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "terminated %s\n", args[0])
			return nil
		},
	}
}

func newDeleteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete <TICKET>",
		Short: "Clear an agent's stored record (archives by default; --hard to purge)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			hard, _ := cmd.Flags().GetBool("hard")
			if err := clientFor(cmd).Delete(cmd.Context(), args[0], hard); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "deleted %s\n", args[0])
			return nil
		},
	}
	cmd.Flags().Bool("hard", false, "permanently purge the record instead of archiving")
	return cmd
}

func newRemoveWorktreeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remove-worktree <TICKET>",
		Short: "Remove an agent's git worktree + branch (always asks; --force overrides guards)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			yes, _ := cmd.Flags().GetBool("yes")
			force, _ := cmd.Flags().GetBool("force")
			deleteAdopted, _ := cmd.Flags().GetBool("delete-adopted-branch")
			if !yes {
				fmt.Fprintf(cmd.OutOrStdout(), "Remove the git worktree and branch for %s? This cannot be undone. [y/N]: ", args[0])
				var ans string
				_, _ = fmt.Fscanln(cmd.InOrStdin(), &ans)
				if ans != "y" && ans != "Y" {
					fmt.Fprintln(cmd.OutOrStdout(), "aborted")
					return nil
				}
			}
			if err := clientFor(cmd).RemoveWorktree(cmd.Context(), args[0], force, deleteAdopted); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "removed worktree for %s\n", args[0])
			return nil
		},
	}
	cmd.Flags().Bool("force", false, "override the alive/uncommitted/unpushed guards")
	cmd.Flags().Bool("delete-adopted-branch", false, "also delete the branch even if warden did not create it (adopted branches are kept by default)")
	cmd.Flags().Bool("yes", false, "skip the confirmation prompt")
	return cmd
}

func newDoneCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "done <TICKET>",
		Short: "Terminate an agent and clear its record (does NOT remove the worktree)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			hard, _ := cmd.Flags().GetBool("hard")
			c := clientFor(cmd)
			if err := c.Terminate(cmd.Context(), args[0]); err != nil {
				return err
			}
			if err := c.Delete(cmd.Context(), args[0], hard); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "done %s (terminated + record cleared; worktree, if any, kept — use remove-worktree)\n", args[0])
			return nil
		},
	}
	cmd.Flags().Bool("hard", false, "purge the record instead of archiving")
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

// currentTmuxSession returns the running tmux session name when invoked inside
// tmux ($TMUX set), else "". A non-empty result selects live-register mode;
// empty selects resume mode.
func currentTmuxSession() string {
	if os.Getenv("TMUX") == "" {
		return ""
	}
	out, err := exec.Command("tmux", "display-message", "-p", "#S").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func newAdoptCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "adopt",
		Short: "Register the Claude session in this directory (resume it under tmux, or register the current tmux session live)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			dirFlag, _ := cmd.Flags().GetString("dir")
			dir, err := resolveDir(dirFlag)
			if err != nil {
				return err
			}
			sessionID, _ := cmd.Flags().GetString("session-id")
			tmuxSession := currentTmuxSession()
			res, err := clientFor(cmd).Adopt(cmd.Context(), client.AdoptParams{
				Cwd: dir, SessionID: sessionID, TmuxSession: tmuxSession,
			})
			if err != nil {
				return err
			}
			mode := "resumed"
			if tmuxSession != "" {
				mode = "live"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "adopted as %s (%s) — attach with `warden attach %s`\n",
				res.Session.ID, mode, res.Session.ID)
			if res.Warning != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "warning: %s\n", res.Warning)
			}
			return nil
		},
	}
	cmd.Flags().String("session-id", "", "claude session uuid to adopt (default: newest for the directory)")
	cmd.Flags().String("dir", "", "directory whose claude session to adopt (default: current directory)")
	return cmd
}
