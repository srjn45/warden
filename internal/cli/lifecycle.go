package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/srjn45/warden/internal/backendstore"
	"github.com/srjn45/warden/internal/client"
	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/preset"
	"github.com/srjn45/warden/internal/prompttemplate"
	"github.com/srjn45/warden/internal/role"
	"github.com/srjn45/warden/internal/store"
	"github.com/srjn45/warden/internal/task"
)

// promptFromArgs returns the prompt for a free-form spawn: the single
// positional argument, or "" when none is given — an empty prompt opens
// claude interactively in the launch dir and waits for instructions.
func promptFromArgs(args []string) string {
	if len(args) == 1 {
		return args[0]
	}
	return ""
}

// formatSpawnOutcome renders the CLI spawn confirmation. Names are mandatory
// after daemon resolution, so the assigned name always appears in parentheses:
// `spawned agent <id> (<name>) [role]` (or `opened interactive agent …`).
func formatSpawnOutcome(s *store.Session, interactive bool) string {
	name := s.Name
	if name == "" {
		name = "unnamed"
	}
	role := store.DisplayRole(s.Role, s.Type)
	if interactive {
		return fmt.Sprintf("opened interactive agent %s (%s) [%s]", s.ID, name, role)
	}
	return fmt.Sprintf("spawned agent %s (%s) [%s]", s.ID, name, role)
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
		Use:   "start --role <ROLE> [TICKET|\"<prompt>\"] [--repo <PATH>] [--dir <PATH>] [--aicli <ID>]",
		Short: "Spawn an agent — `start --role <ROLE> \"<prompt>\"` (free-form), `start --role <ROLE> --dir <path>` (interactive), or `start --role worker --repo <PATH>` (managed worktree)",
		Long: `Spawn an agent. --role is required (see 'warden agent role list'); there is no
implicit fallback role.

Spawn modes:
  Free-form    warden agent start --role <ROLE> "<prompt>" [--dir <path>]
               Autonomous: runs the prompt in --dir (default: current directory).
  Interactive  warden agent start --role <ROLE> --dir <path>
               No prompt: opens the agent and waits for you.
  Managed      warden agent start --role worker --repo <PATH> [TICKET]
               Isolated git worktree off --repo (worker role + --repo).
  Terminal     warden agent start --kind terminal --dir <path>
               Not an AI agent: a plain shell ($SHELL) in --dir, with the same
               worktree/git/tmux lifecycle. --aicli/--model/--role/prompt are ignored.

Which AI CLI + model runs (first match wins):
  1. --aicli + --model         explicit pin (--model requires --aicli)
  2. --aicli alone             optimal model for that AI CLI at the tier
  3. --tier (or --task)        quota-balanced resolver at that tier
  4. --role alone              resolver routed by the role's tier
  5. configured defaults
So --role on its own is always enough; --tier/--aicli/--model only refine it.

Accepted --aicli values: claude (default), aider, opencode, codex, crush, goose,
cursor, antigravity. Per-AI-CLI behaviour (model, resume, spend fidelity) and
maturity labels: see 'warden backend --help'.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Load the named preset first (if any) so its saved defaults seed the
			// flags below; an explicit CLI flag always overrides the preset.
			pre, err := loadStartPreset(cmd)
			if err != nil {
				return err
			}
			typ := stringFlagOr(cmd, "type", pre.Type)
			forkFrom, _ := cmd.Flags().GetString("fork-from")
			repoFlag, _ := cmd.Flags().GetString("repo")

			// --role is mandatory: no implicit fallback to "general". Resolve the
			// built-in role up front so a missing/bad name fails fast with a clear
			// list (the daemon validates too). The role's default flags are applied
			// daemon-side.
			roleName, _ := cmd.Flags().GetString("role")
			if strings.TrimSpace(roleName) == "" {
				return fmt.Errorf("--role is required (valid: %s)", strings.Join(role.Names(), ", "))
			}
			if _, ok := role.Get(roleName); !ok {
				return fmt.Errorf("unknown role %q (valid: %s)", roleName, strings.Join(role.Names(), ", "))
			}
			// A fork is intrinsically managed (§7). Default to worker when the
			// caller did not pick a worktree-owning role so isolation still holds
			// without the deprecated --type development default.
			if forkFrom != "" && !lifecycle.RoleOwnsWorktree(roleName) {
				roleName = "worker"
			}

			// --tier / --task steer the quota-balanced resolver that picks the spawn's
			// AI CLI+model. Validate up front so a typo fails fast rather than being
			// silently ignored (the resolver degrades to defaults on an unknown value).
			tier, _ := cmd.Flags().GetString("tier")
			if tier != "" && !backendstore.ModelTier(tier).Valid() {
				return fmt.Errorf("invalid --tier %q (valid: tier-1, tier-2, tier-3)", tier)
			}
			taskName, _ := cmd.Flags().GetString("task")
			if taskName != "" {
				if _, ok := task.Get(taskName); !ok {
					return fmt.Errorf("unknown --task %q (valid: %s)", taskName, strings.Join(task.Names(), ", "))
				}
			}
			// --model is only meaningful with an explicit AI CLI pin.
			modelFlag := stringFlagOr(cmd, "model", pre.Model)
			aiCliEarly := resolveAiCliFlag(cmd)
			if strings.TrimSpace(modelFlag) != "" && aiCliEarly == "" {
				return fmt.Errorf("--model requires --aicli (aliases: --ai-cli, --backend)")
			}

			// Managed when Type is set (deprecated), fork_from is set, or a
			// worktree-owning role is paired with an explicit --repo.
			managed := typ != "" || forkFrom != "" || (lifecycle.RoleOwnsWorktree(roleName) && repoFlag != "")

			// A prompt template fills the (free-form) spawn prompt; it has no role
			// in managed mode, where the daemon generates the prompt from the ticket.
			if tplName, _ := cmd.Flags().GetString("prompt-template"); tplName != "" && managed {
				return fmt.Errorf("--prompt-template applies to free-form spawns; drop --repo/--type/--fork-from or use the template's prompt directly")
			}

			// Free-form mode: `warden start "<prompt>" [--dir]` (autonomous) or
			// `warden start --dir <path>` with no prompt (interactive).
			if !managed {
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
				aiCli := resolveAiCliFlag(cmd)
				kind, _ := cmd.Flags().GetString("kind")
				tagsFlag, _ := cmd.Flags().GetString("tags")
				projectID, _ := cmd.Flags().GetString("project")
				if projectID == "" {
					if projectID, err = projectIDForDir(dir); err != nil {
						return err
					}
				}
				planID, _ := cmd.Flags().GetString("plan")
				s, err := clientFor(cmd).Spawn(cmd.Context(), client.SpawnParams{Name: name, Prompt: prompt, Cwd: dir, PermissionMode: permissionMode, AutoRestart: autoRestart, Force: force, Model: model, AiCli: aiCli, Backend: aiCli, Kind: kind, Tags: parseTags(tagsFlag), Role: roleName, Tier: tier, Task: taskName, ProjectID: projectID, PlanID: planID, ParentID: os.Getenv("WARDEN_SESSION_ID")})
				if err != nil {
					var cre *client.ErrConfirmationRequired
					if errors.As(err, &cre) {
						fmt.Fprintf(cmd.ErrOrStderr(),
							"⚠ memory pressure: %s\n  re-run with --force to spawn anyway\n", cre.Verdict.Reason)
						return fmt.Errorf("spawn blocked by memory-pressure gate")
					}
					return err
				}
				if jsonRequested(cmd) {
					return printSpawnedJSON(cmd, s)
				}
				outcome := formatSpawnOutcome(s, prompt == "")
				fmt.Fprintf(cmd.OutOrStdout(), "%s — attach with `warden attach %s`\n", outcome, s.ID)
				return nil
			}

			// Managed worktree mode: role+repo (canonical) or deprecated --type.
			repo := repoFlag
			if repo == "" && forkFrom == "" {
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
			aiCli := resolveAiCliFlag(cmd)
			tagsFlag, _ := cmd.Flags().GetString("tags")
			projectID, _ := cmd.Flags().GetString("project")
			if projectID == "" && repo != "" {
				if projectID, err = projectIDForDir(repo); err != nil {
					return err
				}
			}
			planID, _ := cmd.Flags().GetString("plan")
			s, err := clientFor(cmd).Spawn(cmd.Context(), client.SpawnParams{
				Name: name, Type: typ, Ticket: ticket, Repo: repo, Branch: branch, PR: pr, Worktree: worktree, InRepo: inRepo, PermissionMode: permissionMode, AutoRestart: autoRestart, Force: force, Model: model, AiCli: aiCli, Backend: aiCli, Tags: parseTags(tagsFlag), ForkFrom: forkFrom, Role: roleName, Tier: tier, Task: taskName, ProjectID: projectID, PlanID: planID, ParentID: os.Getenv("WARDEN_SESSION_ID"),
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
			if jsonRequested(cmd) {
				return printSpawnedJSON(cmd, s)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s (%s) — attach with `warden attach %s`\n",
				formatSpawnOutcome(s, false), s.Status, s.ID)
			return nil
		},
	}
	addJSONFlag(cmd, "emit the new agent (id, name, role, ai_cli, model, workdir) as JSON")
	cmd.Flags().String("name", "", "explicit agent name (omit to auto-resolve); max 32 chars, alphanumeric + hyphens/underscores")
	cmd.Flags().String("type", "", "deprecated alias: legacy task type (development|analysis|spike|pr-review|…). Prefer --role worker --repo for managed worktrees")
	_ = cmd.Flags().MarkDeprecated("type", "use --role (and --repo for managed worktrees)")
	_ = cmd.Flags().MarkHidden("type")
	cmd.Flags().String("repo", "", "repo path for a managed spawn: with --role worker, picks an isolated worktree off this repo (the default managed mode; branch it with --branch). Empty = free-form unless --fork-from forces managed (then defaults to cwd)")
	cmd.Flags().String("branch", "", "new branch (development) or checkout target (pr-review)")
	cmd.Flags().String("pr", "", "PR number/url (pr-review)")
	cmd.Flags().Bool("worktree", false, "managed spawns only: use a throwaway scratch worktree instead of a branch worktree (analysis/spike). Neither --worktree nor --in-repo = the normal isolated branch worktree")
	cmd.Flags().Bool("in-repo", false, "managed spawns only: opt out of isolation and run in the shared --repo checkout instead of a worktree (ignored for pr-review; overridden when root_guard is on)")
	cmd.Flags().String("dir", "", "directory to launch the agent from (default: current directory)")
	cmd.Flags().Bool("supervised", false, "alias for --permission-mode acceptEdits (kept for backwards compatibility)")
	_ = cmd.Flags().MarkHidden("supervised")
	cmd.Flags().String("permission-mode", "", "permission mode: acceptEdits|auto|bypassPermissions|default|dontAsk|plan (default: from config or 'auto')")
	cmd.Flags().Bool("auto-restart", false, "auto-resume this agent if it crashes (errored), capped at a few attempts")
	cmd.Flags().Bool("force", false, "spawn even when the memory-pressure gate warns")
	cmd.Flags().String("model", "", "model ID for the chosen AI CLI (requires --aicli). Empty lets the tier resolver pick an explicit model")
	cmd.Flags().String("aicli", "", "AI CLI `<ID>`: claude (default, stable) | aider | opencode | codex | crush | goose | cursor | antigravity — only claude is fully tested; codex/antigravity are beta, the rest experimental. See 'warden backend --help' for per-AI-CLI notes")
	cmd.Flags().String("ai-cli", "", "alias for --aicli")
	_ = cmd.Flags().MarkHidden("ai-cli")
	cmd.Flags().String("backend", "", "deprecated alias for --aicli (accepted for one release; --aicli wins if both are set)")
	_ = cmd.Flags().MarkDeprecated("backend", "use --aicli")
	_ = cmd.Flags().MarkHidden("backend")
	cmd.Flags().String("kind", "", "session kind: empty/agent (default) spawns an AI agent; terminal opens a plain interactive shell ($SHELL) in --dir (not an AI agent — --aicli/--model/--role/prompt ignored)")
	cmd.Flags().String("preset", "", "load saved spawn defaults from the named preset `<NAME>` (see 'warden project preset'); explicit flags override")
	cmd.Flags().String("prompt-template", "", "fill the saved prompt template `<NAME>` (see 'warden project prompt-template') as the spawn prompt; a positional prompt still wins")
	cmd.Flags().StringArray("set", nil, "supply a prompt-template variable as VAR=value (repeatable, e.g. --set FILE=foo.go --set X=y)")
	cmd.Flags().String("tags", "", "comma-separated labels `<LIST>` for grouping/filtering (e.g. --tags backend,urgent); searchable and filterable via 'warden agent list --tag'")
	cmd.Flags().String("project", "", "`<ID>` of the daemon project this agent joins (its canonical path or remote URL, from 'warden projects list'); stamps membership explicitly instead of leaving the daemon to path-match the launch dir. Empty = the git repository root of the launch directory (the daemon auto-registers it)")
	cmd.Flags().String("plan", "", "optional planstore plan id in the same project (plan-<8hex>); empty = planless agent. A non-empty value must name an existing plan belonging to the resolved project")
	cmd.Flags().String("role", "", "REQUIRED — built-in agent role `<ROLE>`: "+roleChoices()+". Injects the role's persona as a system-prompt addendum and applies its default flags. See 'warden agent role list'")
	cmd.Flags().String("tier", "", "model tier for the quota-balanced resolver that picks the AI CLI+model: tier-1|tier-2|tier-3. Empty derives the tier from --task, then --role (--role is required, so this always has a role to derive from). An explicit --aicli/--model still wins over the resolver")
	cmd.Flags().String("task", "", "task name (task registry) used to derive the model tier when --tier is empty. Empty = none")
	cmd.Flags().String("fork-from", "", "fork the existing agent `<AGENT>`'s recorded session into this new managed agent (codex's native fork): branches the source's conversation in a fresh sibling worktree off its branch, carrying its uncommitted tracked changes; the source keeps running. Uses --role worker when the chosen role does not own a worktree; the fork inherits the source's repo+backend. See 'warden agent fork' for the shorthand")
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
		return preset.Preset{}, fmt.Errorf("preset %q not found — list saved presets with 'warden project preset list'", name)
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
		return "", fmt.Errorf("prompt template %q not found — list saved templates with 'warden project prompt-template list'", name)
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
		Use:   "restore <AGENT>",
		Short: "Recreate and resume a lost/orphaned agent (resumes its AI CLI session)",
		Long: `Use when warden still has the agent's record but its tmux session is gone
(orphaned: reboot, killed tmux server, crash). Recreates the tmux session in the
agent's original workdir and resumes the same AI CLI conversation. Resume-only:
it refuses rather than start a fresh conversation (e.g. backend cannot resume,
no pinned session id, workdir or transcript missing) and refuses while the tmux
session is still alive.

Not this command?
  warden agent recover   archived orphaned records whose tmux session is still alive
  warden agent adopt     a session warden never managed`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := clientFor(cmd).Restore(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "restoring %s\n", args[0])
			return nil
		},
	}
}

// newRecoverCmd backs `wd recover`: the safety net for the tombstone reaper
// (internal/daemon/tombstone_reap.go). A record can only be archived out from
// under a still-live tmux session by a stale orphaned status (the reaper now
// reconfirms liveness before archiving one, but this covers whatever slips
// through). Spec D8: only archived records whose status is orphaned are
// candidates. Bare `wd recover` only reports candidates (orphaned archives
// whose tmux session is confirmed still alive) and changes nothing; --apply
// re-inserts each one into the active store under its original id, so any
// children (linked via parent_id) reconnect automatically.
func newRecoverCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "recover",
		Short: "Revive archived orphaned agent records whose tmux session is still alive (dry run unless --apply)",
		Long: "Use when an agent vanished from the list but its tmux session is still running\n" +
			"(the record was archived by mistake) — it brings the record back; it never\n" +
			"relaunches anything. If the tmux session is gone, use `warden agent restore`;\n" +
			"for a session warden never managed, use `warden agent adopt`.\n\n" +
			"Scans archived (closed) agent records for ones whose status is orphaned\n" +
			"(the only recovery source) and whose tmux session is confirmed still alive\n" +
			"— a live session's record should never end up archived, but a stale orphaned\n" +
			"status racing a daemon restart could previously slip one past the tombstone\n" +
			"reaper. Bare `wd recover` only reports what it finds; --apply re-inserts each\n" +
			"candidate into the active store under its original id. Any children (linked\n" +
			"via parent_id, untouched by archiving) reconnect automatically — no need to\n" +
			"recover them separately.\n\n" +
			"--apply is a dry-run switch (report vs. act), not a confirmation, so it is\n" +
			"intentionally NOT --yes.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			apply, _ := cmd.Flags().GetBool("apply")
			results, err := clientFor(cmd).Recover(cmd.Context(), apply)
			if err != nil {
				return err
			}
			if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
				if results == nil {
					results = []client.RecoverResult{}
				}
				return printJSON(cmd.OutOrStdout(), results)
			}
			if len(results) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no recoverable agents found (no orphaned archive has a live tmux session)")
				return nil
			}
			for _, r := range results {
				verb := "would recover"
				if apply {
					verb = "recovered"
					if r.Error != "" {
						verb = "FAILED to recover"
					}
				}
				label := r.ID
				if r.Name != "" {
					label = fmt.Sprintf("%s (%s)", r.ID, r.Name)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s: %s — tmux %q, workdir %s\n", verb, label, r.TmuxSession, r.Workdir)
				if r.Error != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "    error: %s\n", r.Error)
				}
			}
			if !apply {
				fmt.Fprintf(cmd.OutOrStdout(), "\n%d candidate(s). Re-run with --apply to recover them.\n", len(results))
			}
			return nil
		},
	}
	cmd.Flags().Bool("apply", false, "actually re-insert candidates (default: report only); a dry-run switch, not a --yes confirmation")
	cmd.Flags().Bool("json", false, "output as JSON")
	return cmd
}

// teardownOpts selects which teardown steps to run and how. The zero value is a
// no-op; callers turn on the steps they want. It backs `stop` and the
// narrower terminate/delete/remove-worktree/done verbs, so every teardown path
// composes the SAME helper.
type teardownOpts struct {
	terminate      bool            // kill the tmux+AI CLI session
	deleteRecord   bool            // clear (archive) the stored record
	removeWorktree bool            // remove the git worktree + branch
	hard           bool            // purge the record instead of archiving
	createPR       bool            // open a GitHub PR first, while the agent is intact
	base           string          // base branch for the PR (only with createPR)
	force          bool            // override the worktree alive/uncommitted/unpushed guards
	deleteAdopted  bool            // also delete an adopted (warden-didn't-create) branch
	yes            bool            // skip the interactive worktree-removal confirmation
	result         *teardownResult // when non-nil, filled with the steps that ran
}

// teardown composes the existing daemon-client calls in the safe order —
// PR (while the agent is still intact) → terminate → remove worktree → delete
// record. The worktree-removal confirmation prompt is asked UP FRONT, before
// any destructive step, so declining leaves the agent fully intact; it is
// gated by opts.yes and only shown when worktree removal is actually requested.
//
// Returns ok=false (with err=nil) when the user declines the confirmation, so
// callers can skip their success summary.
func teardown(cmd *cobra.Command, c *client.Client, id string, o teardownOpts) (ok bool, err error) {
	// Confirm worktree removal before doing anything destructive, so a decline
	// is a true no-op rather than a half-finished teardown.
	if o.removeWorktree && !o.yes && jsonRequested(cmd) {
		return false, requireYesForJSON(cmd, "remove the worktree")
	}
	if o.result != nil {
		o.result.ID = id
		o.result.Steps = []string{}
	}
	if o.removeWorktree && !o.yes {
		fmt.Fprintf(progressOut(cmd), "Remove the git worktree and branch for %s? This cannot be undone. [y/N]: ", id)
		var ans string
		_, _ = fmt.Fscanln(cmd.InOrStdin(), &ans)
		if ans != "y" && ans != "Y" {
			fmt.Fprintln(progressOut(cmd), "aborted")
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
		fmt.Fprintf(progressOut(cmd), "%s: %s\n", verb, res.URL)
		if o.result != nil {
			o.result.PRURL = res.URL
			o.result.Steps = append(o.result.Steps, "pr")
		}
	}
	// Order: terminate → remove worktree → clear record. The daemon resolves the
	// session by its record, so the worktree must go while it still resolves; a
	// failed guard then leaves the record intact and the call retryable.
	var done []string
	fail := func(step string, err error) (bool, error) {
		ran := "nothing"
		if len(done) > 0 {
			ran = strings.Join(done, ", ")
		}
		return false, fmt.Errorf("%s failed: %w\n(completed: %s; record left intact — fix the cause and retry)", step, err, ran)
	}
	if o.terminate {
		if err := c.Terminate(cmd.Context(), id); err != nil {
			return fail("terminate", err)
		}
		done = append(done, "terminated")
	}
	if o.removeWorktree {
		if err := c.RemoveWorktree(cmd.Context(), id, o.force, o.deleteAdopted); err != nil {
			return fail("remove worktree", err)
		}
		done = append(done, "worktree removed")
	}
	if o.deleteRecord {
		if err := c.Delete(cmd.Context(), id, o.hard); err != nil {
			return fail("clear record", err)
		}
		done = append(done, "record cleared")
	}
	if o.result != nil {
		o.result.Steps = append(o.result.Steps, done...)
	}
	return true, nil
}

func newStopCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stop <AGENT>",
		Short: "Tear down an agent (default: terminate + clear record + remove worktree; --keep-* flags keep parts)",
		Long: `Stop an agent: the default, full teardown.

<AGENT> is any identifier ` + "`wd ls`" + ` shows — the agent's name, its id, or its
ticket.

By default ` + "`wd agent stop <AGENT>`" + ` terminates the tmux+AI CLI session, clears
(archives) the record, and removes the git worktree + branch (asking for
confirmation first, unless --yes). Keep parts around with:

  --keep-worktree   leave the git worktree and branch in place
  --keep-record     leave the stored record in place
  --hard            purge the record instead of archiving it
  --pr              open a GitHub PR first, while the agent is still intact

Safe ordering is always: PR -> terminate -> remove worktree -> clear record, so
a failed push leaves the agent running and a failed worktree guard (alive /
dirty / unpushed) leaves the record intact and the call retryable. A failure
reports which steps already ran.

To only kill the session and keep everything else, use
` + "`wd agent terminate <AGENT>`" + ` (same as stop --keep-record --keep-worktree).

Older verbs (hidden, still work; not all are expressible as stop flags):

  wd agent done <A> [--hard|--pr]  terminate + clear record, worktree kept
                                   (stop --keep-worktree [--hard|--pr])
  wd agent delete <A> [--hard]     clear the record ONLY; does not terminate
                                   (no stop equivalent)
  wd agent remove-worktree <A>     remove the worktree + branch ONLY; does not
                                   terminate (no stop equivalent)`,
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
			res := &teardownResult{}
			ok, err := teardown(cmd, clientFor(cmd), args[0], teardownOpts{
				result:         res,
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
			if jsonRequested(cmd) {
				return printJSON(cmd.OutOrStdout(), res)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "stopped %s\n", args[0])
			return nil
		},
	}
	addJSONFlag(cmd, "emit the teardown result (id, steps that ran) as JSON; never prompts — remove-worktree needs --yes")
	cmd.Flags().Bool("keep-record", false, "do not clear the stored record")
	cmd.Flags().Bool("keep-worktree", false, "do not remove the git worktree and branch")
	cmd.Flags().Bool("hard", false, "purge the record instead of archiving")
	cmd.Flags().Bool("pr", false, "open a GitHub PR for the agent's branch (pushes first; title+body from the digest) before tearing down")
	cmd.Flags().String("base", "", "base branch for the PR (default main); only meaningful with --pr")
	cmd.Flags().Bool("yes", false, "skip the worktree-removal confirmation prompt")
	cmd.Flags().Bool("force", false, "override the alive/uncommitted/unpushed worktree guards")
	cmd.Flags().Bool("delete-adopted-branch", false, "also delete the branch even if warden did not create it (adopted branches are kept by default)")
	return cmd
}

// newTerminateCmd kills the session only; same as `stop --keep-record --keep-worktree`.
func newTerminateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "terminate <AGENT>",
		Short: "Stop an agent: kill its tmux+AI CLI session (keeps the record and worktree)",
		Long:  "Stop an agent: kill its tmux+AI CLI session (keeps the record and worktree).\n\nTerminating the manager of an active autopilot run is not a stop: the guardian\nrespawns it in the same slot. To stop a run use `wd plan pause` or `wd plan stop`.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			res := &teardownResult{}
			ok, err := teardown(cmd, clientFor(cmd), args[0], teardownOpts{terminate: true, result: res})
			if err != nil || !ok {
				return err
			}
			if jsonRequested(cmd) {
				return printJSON(cmd.OutOrStdout(), res)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "terminated %s\n", args[0])
			return nil
		},
	}
	addJSONFlag(cmd, "emit the teardown result (id, steps that ran) as JSON")
	return cmd
}

// newDeleteCmd clears only the stored record. It does not terminate the session
// or touch the worktree, so it is not expressible as `stop` flags. Hidden: use stop.
func newDeleteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "delete <AGENT>",
		Short:  "Clear only an agent's stored record (archives by default; --hard to purge); does not terminate it or touch the worktree",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			hard, _ := cmd.Flags().GetBool("hard")
			res := &teardownResult{}
			ok, err := teardown(cmd, clientFor(cmd), args[0], teardownOpts{deleteRecord: true, hard: hard, result: res})
			if err != nil || !ok {
				return err
			}
			if jsonRequested(cmd) {
				return printJSON(cmd.OutOrStdout(), res)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "deleted %s\n", args[0])
			return nil
		},
	}
	addJSONFlag(cmd, "emit the teardown result (id, steps that ran) as JSON; never prompts — remove-worktree needs --yes")
	cmd.Flags().Bool("hard", false, "permanently purge the record instead of archiving")
	return cmd
}

// newRemoveWorktreeCmd removes only the worktree + branch. It does not terminate
// the session or clear the record, so it is not expressible as `stop` flags.
// Hidden: use stop.
func newRemoveWorktreeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "remove-worktree <AGENT>",
		Short:  "Remove only an agent's git worktree + branch (asks first unless --yes; --force overrides guards); does not terminate it or clear the record",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			yes, _ := cmd.Flags().GetBool("yes")
			force, _ := cmd.Flags().GetBool("force")
			deleteAdopted, _ := cmd.Flags().GetBool("delete-adopted-branch")
			res := &teardownResult{}
			ok, err := teardown(cmd, clientFor(cmd), args[0], teardownOpts{
				result:         res,
				removeWorktree: true, force: force, deleteAdopted: deleteAdopted, yes: yes,
			})
			if err != nil || !ok {
				return err
			}
			if jsonRequested(cmd) {
				return printJSON(cmd.OutOrStdout(), res)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "removed worktree for %s\n", args[0])
			return nil
		},
	}
	cmd.Flags().Bool("force", false, "override the alive/uncommitted/unpushed guards")
	cmd.Flags().Bool("delete-adopted-branch", false, "also delete the branch even if warden did not create it (adopted branches are kept by default)")
	addJSONFlag(cmd, "emit the teardown result (id, steps that ran) as JSON; never prompts — remove-worktree needs --yes")
	cmd.Flags().Bool("yes", false, "skip the confirmation prompt")
	return cmd
}

// newDoneCmd terminates the session and clears the record, keeping the worktree
// (same as `stop --keep-worktree`), with the PR-first ordering. Hidden: use stop.
func newDoneCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "done <AGENT>",
		Short:  "Terminate an agent and clear its record, keeping the worktree (same as `stop --keep-worktree`)",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			hard, _ := cmd.Flags().GetBool("hard")
			createPR, _ := cmd.Flags().GetBool("pr")
			if legacy, _ := cmd.Flags().GetBool("create-pr"); legacy {
				createPR = true
			}
			base, _ := cmd.Flags().GetString("base")
			res := &teardownResult{}
			ok, err := teardown(cmd, clientFor(cmd), args[0], teardownOpts{
				result:    res,
				terminate: true, deleteRecord: true, hard: hard, createPR: createPR, base: base,
			})
			if err != nil || !ok {
				return err
			}
			if jsonRequested(cmd) {
				return printJSON(cmd.OutOrStdout(), res)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "done %s (terminated + record cleared; worktree, if any, kept — use remove-worktree)\n", args[0])
			return nil
		},
	}
	addJSONFlag(cmd, "emit the teardown result (id, steps that ran) as JSON; never prompts — remove-worktree needs --yes")
	cmd.Flags().Bool("hard", false, "purge the record instead of archiving")
	cmd.Flags().Bool("pr", false, "open a GitHub PR for the agent's branch (pushes first; title+body drafted by Fast-Brain when available, else from the digest) before finishing")
	cmd.Flags().Bool("create-pr", false, "alias for --pr")
	_ = cmd.Flags().MarkHidden("create-pr")
	cmd.Flags().String("base", "", "base branch for the PR (default main); only meaningful with --pr")
	return cmd
}

func newAttachCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "attach <AGENT>",
		Short: "Attach to the agent's tmux session",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Replaces the current process with an interactive tmux attach.
			tmux, err := exec.LookPath("tmux")
			if err != nil {
				return err
			}
			// Resolve name/id/ticket to the tmux session via the daemon; fall back
			// to the raw argument (a tmux session name) if the daemon cannot.
			target := args[0]
			if sess, gerr := clientFor(cmd).Get(cmd.Context(), target); gerr == nil && sess.TmuxSession != "" {
				target = sess.TmuxSession
			}
			c := exec.Command(tmux, "attach", "-t", target)
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
		Short: "Register the AI CLI session in this directory (resume it under tmux, or register the current tmux session live)",
		Long: `Use for an AI CLI session warden never spawned or tracked — no existing record.
Registers it and returns a new warden agent id. Run from inside a tmux session it
adopts that session live (no relaunch); otherwise it resumes the newest session
for --dir (or --session-id) under a fresh tmux session.

Not this command?
  warden agent restore   a known warden record whose tmux session is gone
  warden agent recover   archived orphaned records whose tmux session is still alive`,
		Args: cobra.NoArgs,
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
	cmd.Flags().String("session-id", "", "AI CLI session id to adopt (default: newest for the directory)")
	cmd.Flags().String("dir", "", "directory whose AI CLI session to adopt (default: current directory)")
	return cmd
}

// resolveAiCliFlag returns the AI CLI id from --aicli (canonical), the --ai-cli
// alias, or the deprecated --backend alias. Precedence: --aicli > --ai-cli > --backend.
func resolveAiCliFlag(cmd *cobra.Command) string {
	if aicli, _ := cmd.Flags().GetString("aicli"); strings.TrimSpace(aicli) != "" {
		return strings.TrimSpace(aicli)
	}
	if ai, _ := cmd.Flags().GetString("ai-cli"); strings.TrimSpace(ai) != "" {
		return strings.TrimSpace(ai)
	}
	backend, _ := cmd.Flags().GetString("backend")
	return strings.TrimSpace(backend)
}
