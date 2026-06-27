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
	"github.com/srjn45/warden/internal/preset"
	"github.com/srjn45/warden/internal/prompttemplate"
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

// parseTags splits the comma-separated --tags flag into individual labels. Each
// is trimmed and blanks are dropped; the daemon normalizes (lowercase + dedup)
// before persisting, so `--tags "Backend, backend,"` yields one tag "backend".
func parseTags(flag string) []string {
	if flag == "" {
		return nil
	}
	var tags []string
	for _, t := range strings.Split(flag, ",") {
		if t = strings.TrimSpace(t); t != "" {
			tags = append(tags, t)
		}
	}
	return tags
}

func newStartCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "start [TICKET|\"<prompt>\"] [--type <TYPE>] [--dir <PATH>] [--backend <ID>]",
		Short: "Spawn an agent — `start \"<prompt>\"` (auto-typed), `start --dir <path>` (interactive: open Claude & wait), or `start TICKET --type <TYPE>` (managed worktree)",
		Long: `Spawn an agent.

Free-form:   warden start "<prompt>" [--dir <path>]   (autonomous)
Interactive: warden start --dir <path>                (opens the agent and waits)
Managed:     warden start TICKET --type <TYPE>        (isolated worktree)

Backends (--backend): warden drives Claude Code by default. Pass --backend aider
to drive Aider instead. Backends differ in capabilities (design §5): Aider is
bring-your-own-model (pass --model, e.g. ollama_chat/qwen2.5-coder:3b), has no
resume and no priced spend (tokens only), and runs an autonomous --message task
that exits when done rather than a persistent loop. Claude remains full-fidelity.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Load the named preset first (if any) so its saved defaults seed the
			// flags below; an explicit CLI flag always overrides the preset.
			pre, err := loadStartPreset(cmd)
			if err != nil {
				return err
			}
			typ := stringFlagOr(cmd, "type", pre.Type)

			// A prompt template fills the (free-form) spawn prompt; it has no role
			// in typed mode, where the daemon generates the prompt from the ticket.
			if tplName, _ := cmd.Flags().GetString("prompt-template"); tplName != "" && typ != "" {
				return fmt.Errorf("--prompt-template applies to free-form spawns; drop --type or use the template's prompt directly")
			}

			// Free-form mode: `warden start "<prompt>" [--dir]` (autonomous) or
			// `warden start --dir <path>` with no prompt (interactive: opens
			// claude in the dir and waits). No --type.
			if typ == "" {
				prompt, err := resolveStartPrompt(cmd, args)
				if err != nil {
					return err
				}
				dirFlag, _ := cmd.Flags().GetString("dir")
				dir, err := resolveDir(dirFlag)
				if err != nil {
					return err
				}
				name, _ := cmd.Flags().GetString("name")
				supervised, _ := cmd.Flags().GetBool("supervised")
				permissionMode := stringFlagOr(cmd, "permission-mode", pre.PermissionMode)
				// --supervised is an alias for --permission-mode acceptEdits
				if supervised && permissionMode == "" {
					permissionMode = "acceptEdits"
				}
				autoRestart := boolFlagOr(cmd, "auto-restart", pre.AutoRestart)
				force, _ := cmd.Flags().GetBool("force")
				model := stringFlagOr(cmd, "model", pre.Model)
				backend, _ := cmd.Flags().GetString("backend")
				tagsFlag, _ := cmd.Flags().GetString("tags")
				s, err := clientFor(cmd).Spawn(cmd.Context(), client.SpawnParams{Name: name, Prompt: prompt, Cwd: dir, PermissionMode: permissionMode, AutoRestart: autoRestart, Force: force, Model: model, Backend: backend, Tags: parseTags(tagsFlag)})
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
			worktree := boolFlagOr(cmd, "worktree", pre.Worktree)
			inRepo := boolFlagOr(cmd, "in-repo", pre.InRepo)
			supervised, _ := cmd.Flags().GetBool("supervised")
			permissionMode := stringFlagOr(cmd, "permission-mode", pre.PermissionMode)
			// --supervised is an alias for --permission-mode acceptEdits
			if supervised && permissionMode == "" {
				permissionMode = "acceptEdits"
			}
			autoRestart := boolFlagOr(cmd, "auto-restart", pre.AutoRestart)
			if typ == "pr-review" && pr == "" && branch == "" {
				return fmt.Errorf("pr-review needs --pr or --branch")
			}
			ticket := ""
			if len(args) == 1 {
				ticket = args[0]
			}
			force, _ := cmd.Flags().GetBool("force")
			model := stringFlagOr(cmd, "model", pre.Model)
			backend, _ := cmd.Flags().GetString("backend")
			tagsFlag, _ := cmd.Flags().GetString("tags")
			s, err := clientFor(cmd).Spawn(cmd.Context(), client.SpawnParams{
				Name: name, Type: typ, Ticket: ticket, Repo: repo, Branch: branch, PR: pr, Worktree: worktree, InRepo: inRepo, PermissionMode: permissionMode, AutoRestart: autoRestart, Force: force, Model: model, Backend: backend, Tags: parseTags(tagsFlag),
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
	cmd.Flags().Bool("in-repo", false, "write-agent opt-out: run in the shared repo instead of an isolated worktree (ignored for pr-review)")
	cmd.Flags().String("dir", "", "directory to launch the agent from (default: current directory)")
	cmd.Flags().Bool("supervised", false, "alias for --permission-mode acceptEdits (kept for backwards compatibility)")
	cmd.Flags().String("permission-mode", "", "permission mode: acceptEdits|auto|bypassPermissions|default|dontAsk|plan (default: from config or 'auto')")
	cmd.Flags().Bool("auto-restart", false, "auto-resume this agent if it crashes (errored), capped at a few attempts")
	cmd.Flags().Bool("force", false, "spawn even when the memory-pressure gate warns")
	cmd.Flags().String("model", "", "claude model: opus, sonnet, haiku, fable, or full model ID (default: the model_default config setting, i.e. sonnet)")
	cmd.Flags().String("backend", "", "agent backend to drive: claude (default) or aider. Backends differ in capabilities — see `warden` docs (e.g. aider is bring-your-own-model: pass --model)")
	cmd.Flags().String("preset", "", "load saved spawn defaults from a named preset (see `warden preset`); explicit flags override")
	cmd.Flags().String("prompt-template", "", "fill a saved prompt template (see `warden prompt-template`) as the spawn prompt; a positional prompt still wins")
	cmd.Flags().StringArray("set", nil, "supply a prompt-template variable as VAR=value (repeatable, e.g. --set FILE=foo.go --set X=y)")
	cmd.Flags().String("tags", "", "comma-separated labels for grouping/filtering (e.g. --tags backend,urgent); searchable and filterable via `warden ls --tag`")
	return cmd
}

// loadStartPreset resolves the --preset flag to its saved spawn defaults. An
// empty flag yields a zero Preset (no defaults). A named-but-missing preset is
// an error, so a typo doesn't silently fall through to bare defaults.
func loadStartPreset(cmd *cobra.Command) (preset.Preset, error) {
	name, _ := cmd.Flags().GetString("preset")
	if name == "" {
		return preset.Preset{}, nil
	}
	store, err := preset.Load(presetPathFor(cmd))
	if err != nil {
		return preset.Preset{}, err
	}
	p, ok := store.Get(name)
	if !ok {
		return preset.Preset{}, fmt.Errorf("preset %q not found — list saved presets with `warden preset list`", name)
	}
	return p, nil
}

// resolveStartPrompt determines the free-form spawn prompt. An explicit
// positional prompt always wins; otherwise, when --prompt-template is given, the
// named template is loaded and its `{{VAR}}` placeholders filled from --set
// VAR=value pairs. With neither, the prompt is "" (interactive spawn).
func resolveStartPrompt(cmd *cobra.Command, args []string) (string, error) {
	if p := promptFromArgs(args); p != "" {
		return p, nil
	}
	name, _ := cmd.Flags().GetString("prompt-template")
	if name == "" {
		return "", nil
	}
	store, err := prompttemplate.Load(promptTemplatePathFor(cmd))
	if err != nil {
		return "", err
	}
	tpl, ok := store.Get(name)
	if !ok {
		return "", fmt.Errorf("prompt template %q not found — list saved templates with `warden prompt-template list`", name)
	}
	sets, _ := cmd.Flags().GetStringArray("set")
	vars, err := parseSetVars(sets)
	if err != nil {
		return "", err
	}
	return tpl.Resolve(vars)
}

// parseSetVars turns repeated `--set VAR=value` flags into a name→value map.
// The value may itself contain `=` (only the first separator splits); an empty
// or `=`-less entry is rejected so a malformed --set surfaces immediately.
func parseSetVars(sets []string) (map[string]string, error) {
	vars := make(map[string]string, len(sets))
	for _, s := range sets {
		k, v, ok := strings.Cut(s, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("invalid --set %q: expected VAR=value", s)
		}
		vars[k] = v
	}
	return vars, nil
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

// teardownOpts selects which teardown steps to run and how. The zero value is a
// no-op; callers turn on the steps they want. It backs the single `stop`
// umbrella command AND its four thin-wrapper aliases (terminate/delete/
// remove-worktree/done), so every teardown path composes the SAME helper.
type teardownOpts struct {
	terminate      bool   // kill the tmux+claude session
	deleteRecord   bool   // clear (archive) the stored record
	removeWorktree bool   // remove the git worktree + branch
	hard           bool   // purge the record instead of archiving
	createPR       bool   // open a GitHub PR first, while the agent is intact
	base           string // base branch for the PR (only with createPR)
	force          bool   // override the worktree alive/uncommitted/unpushed guards
	deleteAdopted  bool   // also delete an adopted (warden-didn't-create) branch
	yes            bool   // skip the interactive worktree-removal confirmation
}

// teardown composes the existing daemon-client calls in the safe order —
// PR (while the agent is still intact) → terminate → delete record → remove
// worktree. The worktree-removal confirmation prompt is asked UP FRONT, before
// any destructive step, so declining leaves the agent fully intact; it is
// gated by opts.yes and only shown when worktree removal is actually requested.
//
// Returns ok=false (with err=nil) when the user declines the confirmation, so
// callers can skip their success summary.
func teardown(cmd *cobra.Command, c *client.Client, id string, o teardownOpts) (ok bool, err error) {
	// Confirm worktree removal before doing anything destructive, so a decline
	// is a true no-op rather than a half-finished teardown.
	if o.removeWorktree && !o.yes {
		fmt.Fprintf(cmd.OutOrStdout(), "Remove the git worktree and branch for %s? This cannot be undone. [y/N]: ", id)
		var ans string
		_, _ = fmt.Fscanln(cmd.InOrStdin(), &ans)
		if ans != "y" && ans != "Y" {
			fmt.Fprintln(cmd.OutOrStdout(), "aborted")
			return false, nil
		}
	}
	// Open the PR first, while the agent is still intact: if anything fails
	// (dirty push, protected branch, no gh) the agent is left running so the
	// operator can fix it and retry, rather than losing the session.
	if o.createPR {
		res, err := c.CreatePR(cmd.Context(), id, o.base)
		if err != nil {
			return false, fmt.Errorf("create PR: %w\n(agent left running — fix the issue and retry, or re-run without opening a PR)", err)
		}
		verb := "opened PR"
		if !res.Created {
			verb = "PR already exists"
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s: %s\n", verb, res.URL)
	}
	if o.terminate {
		if err := c.Terminate(cmd.Context(), id); err != nil {
			return false, err
		}
	}
	if o.deleteRecord {
		if err := c.Delete(cmd.Context(), id, o.hard); err != nil {
			return false, err
		}
	}
	if o.removeWorktree {
		if err := c.RemoveWorktree(cmd.Context(), id, o.force, o.deleteAdopted); err != nil {
			return false, err
		}
	}
	return true, nil
}

func newStopCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stop <TICKET>",
		Short: "Tear down an agent — the single umbrella verb (default: terminate + clear record + remove worktree)",
		Long: `Stop an agent. The single umbrella teardown verb.

By default ` + "`wd stop <TICKET>`" + ` does a FULL teardown: terminate the
tmux+claude session, clear (archive) the record, and remove the git worktree +
branch (asking for confirmation first, unless --yes). Subtractive flags keep
parts around; --pr opens a GitHub PR first while the agent is still intact.

The four older verbs are kept as thin aliases — each is just ` + "`stop`" + ` with a
fixed flag combo:

  old verb                    equivalent
  --------------------------  ------------------------------------------------
  wd terminate <T>            wd stop <T> --keep-record --keep-worktree
  wd delete <T> [--hard]      wd stop <T> --keep-worktree (record only)
  wd remove-worktree <T>      wd stop <T> --keep-record  (worktree only)
  wd done <T> [--hard|--pr]   wd stop <T> --keep-worktree [--hard|--pr]
  wd stop <T>                 terminate + clear record + remove worktree

Safe ordering is always: PR -> terminate -> clear record -> remove worktree, so
a failed push leaves the agent running.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			keepRecord, _ := cmd.Flags().GetBool("keep-record")
			keepWorktree, _ := cmd.Flags().GetBool("keep-worktree")
			hard, _ := cmd.Flags().GetBool("hard")
			pr, _ := cmd.Flags().GetBool("pr")
			base, _ := cmd.Flags().GetString("base")
			yes, _ := cmd.Flags().GetBool("yes")
			force, _ := cmd.Flags().GetBool("force")
			deleteAdopted, _ := cmd.Flags().GetBool("delete-adopted-branch")
			ok, err := teardown(cmd, clientFor(cmd), args[0], teardownOpts{
				terminate:      true,
				deleteRecord:   !keepRecord,
				removeWorktree: !keepWorktree,
				hard:           hard,
				createPR:       pr,
				base:           base,
				force:          force,
				deleteAdopted:  deleteAdopted,
				yes:            yes,
			})
			if err != nil || !ok {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "stopped %s\n", args[0])
			return nil
		},
	}
	cmd.Flags().Bool("keep-record", false, "do not clear the stored record")
	cmd.Flags().Bool("keep-worktree", false, "do not remove the git worktree (this + default == the old 'done')")
	cmd.Flags().Bool("hard", false, "purge the record instead of archiving")
	cmd.Flags().Bool("pr", false, "open a GitHub PR for the agent's branch (pushes first; title+body from the digest) before tearing down")
	cmd.Flags().String("base", "", "base branch for the PR (default main); only meaningful with --pr")
	cmd.Flags().Bool("yes", false, "skip the worktree-removal confirmation prompt")
	cmd.Flags().Bool("force", false, "override the alive/uncommitted/unpushed worktree guards")
	cmd.Flags().Bool("delete-adopted-branch", false, "also delete the branch even if warden did not create it (adopted branches are kept by default)")
	return cmd
}

// newTerminateCmd is a thin alias for `stop --keep-record --keep-worktree`.
func newTerminateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "terminate <TICKET>",
		Short: "Stop an agent: kill its tmux+claude session (keeps the record and worktree) — alias for `stop --keep-record --keep-worktree`",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ok, err := teardown(cmd, clientFor(cmd), args[0], teardownOpts{terminate: true})
			if err != nil || !ok {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "terminated %s\n", args[0])
			return nil
		},
	}
}

// newDeleteCmd is a thin alias for `stop` that only clears the record.
func newDeleteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete <TICKET>",
		Short: "Clear an agent's stored record (archives by default; --hard to purge) — alias for `stop --keep-worktree` (record only)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			hard, _ := cmd.Flags().GetBool("hard")
			ok, err := teardown(cmd, clientFor(cmd), args[0], teardownOpts{deleteRecord: true, hard: hard})
			if err != nil || !ok {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "deleted %s\n", args[0])
			return nil
		},
	}
	cmd.Flags().Bool("hard", false, "permanently purge the record instead of archiving")
	return cmd
}

// newRemoveWorktreeCmd is a thin alias for `stop --keep-record` (worktree only),
// preserving the always-ask confirmation prompt.
func newRemoveWorktreeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remove-worktree <TICKET>",
		Short: "Remove an agent's git worktree + branch (always asks; --force overrides guards) — alias for `stop --keep-record` (worktree only)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			yes, _ := cmd.Flags().GetBool("yes")
			force, _ := cmd.Flags().GetBool("force")
			deleteAdopted, _ := cmd.Flags().GetBool("delete-adopted-branch")
			ok, err := teardown(cmd, clientFor(cmd), args[0], teardownOpts{
				removeWorktree: true, force: force, deleteAdopted: deleteAdopted, yes: yes,
			})
			if err != nil || !ok {
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

// newDoneCmd is a thin alias for `stop --keep-worktree`: terminate + clear the
// record while keeping the worktree, with the PR-first ordering.
func newDoneCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "done <TICKET>",
		Short: "Terminate an agent and clear its record (does NOT remove the worktree) — alias for `stop --keep-worktree`",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			hard, _ := cmd.Flags().GetBool("hard")
			createPR, _ := cmd.Flags().GetBool("create-pr")
			base, _ := cmd.Flags().GetString("base")
			ok, err := teardown(cmd, clientFor(cmd), args[0], teardownOpts{
				terminate: true, deleteRecord: true, hard: hard, createPR: createPR, base: base,
			})
			if err != nil || !ok {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "done %s (terminated + record cleared; worktree, if any, kept — use remove-worktree)\n", args[0])
			return nil
		},
	}
	cmd.Flags().Bool("hard", false, "purge the record instead of archiving")
	cmd.Flags().Bool("create-pr", false, "open a GitHub PR for the agent's branch (pushes first; title+body from the digest) before finishing")
	cmd.Flags().String("base", "", "base branch for the PR (default main); only meaningful with --create-pr")
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
