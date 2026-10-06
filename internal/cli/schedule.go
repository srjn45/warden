package cli

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/srjn45/warden/internal/backendstore"
	"github.com/srjn45/warden/internal/client"
	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/role"
	"github.com/srjn45/warden/internal/schedule"
)

func newScheduleCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "schedule",
		Short: "Schedule recurring (--cron) or single-shot (--at) agents and pipelines",
		Long: "Create timer-driven triggers that the daemon fires on a schedule: a recurring\n" +
			"cron spec (--cron \"0 9 * * *\") or a single-shot time (--at 2026-06-27T09:00).\n" +
			"Each schedule fires either one agent spawn (the default — pass --repo/\n" +
			"--prompt) or a pipeline (--pipeline <spec.yaml>). The scheduler is opt-in: set\n" +
			"scheduler_enabled: true in the config file and keep the daemon running.",
	}
	SetCommandHelpMetadata(cmd, "run", 40, "warden schedule", "", NodeNamespace)
	children := []*cobra.Command{
		newScheduleCreateCmd(), newScheduleListCmd(), newScheduleShowCmd(),
		newScheduleEnableCmd(), newScheduleDisableCmd(), newScheduleDeleteCmd(),
	}
	for i, child := range children {
		SetCommandHelpMetadata(child, "run", (i+1)*10, "warden schedule "+child.Name(), "", NodeLeaf)
		cmd.AddCommand(child)
	}
	legacyGet := newScheduleGetCmd()
	legacyGet.Hidden = true
	SetCommandHelpMetadata(legacyGet, "run", 900, "warden schedule show", AliasCompatibility, NodeLeaf)
	cmd.AddCommand(legacyGet)
	return cmd
}

func newScheduleShowCmd() *cobra.Command {
	cmd := newScheduleGetCmd()
	cmd.Use = "show <id>"
	return cmd
}

// scheduleAgentFlags are the options that only apply to an agent schedule, in
// the order a conflict is reported. ai-cli is the hidden alias of aicli.
var scheduleAgentFlags = []string{
	"prompt", "repo", "cwd", "role", "agent", "branch", "model", "aicli", "ai-cli",
	"permission-mode", "auto-restart", "tags", "tier", "project",
}

func newScheduleCreateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create <name> (--cron <spec> | --at <time> | --now) [--prompt <s>] [--role <role>] [--cwd <dir> | --repo <path>] | --pipeline <spec.yaml>",
		Short: "Create a schedule that fires an agent or a pipeline",
		Long: "Create a recurring (--cron), single-shot (--at) or immediate (--now) schedule. By default a\n" +
			"schedule fires one agent, started the same way `warden start` would start it\n" +
			"with the same flags from the directory you run this in:\n" +
			"  --cwd <dir>    launch the agent in this existing directory (default: the\n" +
			"                 current directory, stored as an absolute path)\n" +
			"  --repo <path>  run the agent in an isolated worktree off this repo\n" +
			"                 (--branch picks the branch); replaces the --cwd default\n" +
			"  --role <role>  agent role, see `warden agent role list` (default: worker\n" +
			"                 with --repo, otherwise general)\n" +
			"  --prompt, --agent <name>   the agent's task and optional name\n" +
			"  --aicli <id>, --model <id>   the AI CLI and model to run (--model needs --aicli)\n" +
			"  --permission-mode, --auto-restart, --tags, --tier   as for `warden start`\n" +
			"  --project <id> the project the agent joins (default: the one owning its directory)\n" +
			"A schedule that could never fire (unknown role, missing directory, no\n" +
			"prompt) is rejected with the reason.\n\n" +
			"Pass --pipeline <spec.yaml> instead to fire a pipeline (its name is\n" +
			"timestamp-suffixed per fire so recurring runs don't collide). A pipeline runs as\n" +
			"written, so combining --pipeline with any agent option is an error.\n\n" +
			"Provide exactly one of --cron/--at/--now: --at must be in the future (a past\n" +
			"time is rejected) and --now fires once as soon as possible. --json prints the\n" +
			"created schedule.\n\n" +
			"Examples:\n" +
			"  # recurring agent: a weekday-morning review in the current directory\n" +
			"  warden schedule create morning-review --cron \"0 9 * * 1-5\" --role reviewer \\\n" +
			"      --prompt \"review yesterday's merged PRs and list follow-ups\"\n" +
			"  # single-shot agent: a worktree off a repo, once, at a set time\n" +
			"  warden schedule create release-prep --at 2026-12-01T08:00 --repo ~/dev/app \\\n" +
			"      --aicli claude --model sonnet --prompt \"prepare the release notes\"\n" +
			"  # recurring pipeline: a nightly spec file\n" +
			"  warden schedule create nightly --cron \"@daily\" --pipeline nightly.yaml",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cron, _ := cmd.Flags().GetString("cron")
			at, _ := cmd.Flags().GetString("at")
			pipelineFile, _ := cmd.Flags().GetString("pipeline")
			now, _ := cmd.Flags().GetBool("now")
			if now && (cron != "" || at != "") {
				return fmt.Errorf("--now cannot be combined with --cron or --at")
			}
			req := client.ScheduleCreateRequest{
				Name: args[0],
				Cron: cron,
				At:   at,
				Now:  now,
			}
			var pipelineName string
			var pipelineJobs int
			if pipelineFile != "" {
				var set []string
				for _, f := range scheduleAgentFlags {
					if cmd.Flags().Changed(f) {
						set = append(set, "--"+f)
					}
				}
				if len(set) > 0 {
					return fmt.Errorf("--pipeline fires the pipeline as written and cannot be combined with %s (those apply to an agent schedule); drop them or drop --pipeline", strings.Join(set, ", "))
				}
				data, err := os.ReadFile(pipelineFile)
				if err != nil {
					return err
				}
				p, err := pipeline.ParseSpec(data)
				if err != nil {
					return fmt.Errorf("invalid pipeline spec %s: %w", pipelineFile, err)
				}
				pipelineName, pipelineJobs = p.Name, userJobs(p)
				req.Spec = string(data)
			} else {
				req.Repo, _ = cmd.Flags().GetString("repo")
				req.Role, _ = cmd.Flags().GetString("role")
				if req.Role != "" {
					if _, ok := role.Get(req.Role); !ok {
						return fmt.Errorf("unknown role %q (valid: %s)", req.Role, strings.Join(role.Names(), ", "))
					}
				}
				req.AiCli = resolveAiCliFlag(cmd)
				req.Model, _ = cmd.Flags().GetString("model")
				if strings.TrimSpace(req.Model) != "" && req.AiCli == "" {
					return fmt.Errorf("--model requires --aicli (alias: --ai-cli)")
				}
				req.Tier, _ = cmd.Flags().GetString("tier")
				if req.Tier != "" && !backendstore.ModelTier(req.Tier).Valid() {
					return fmt.Errorf("invalid --tier %q (valid: tier-1, tier-2, tier-3)", req.Tier)
				}
				req.PermissionMode, _ = cmd.Flags().GetString("permission-mode")
				req.AutoRestart, _ = cmd.Flags().GetBool("auto-restart")
				tagsFlag, _ := cmd.Flags().GetString("tags")
				req.Tags = parseTags(tagsFlag)
				// Mirror `warden start`: no --repo means a free-form agent launched in
				// the directory the command is run from (resolved here, not by the
				// daemon, whose own working directory is unrelated).
				cwdFlag, _ := cmd.Flags().GetString("cwd")
				freeForm := req.Role != "" && !lifecycle.RoleOwnsWorktree(req.Role)
				if cwdFlag != "" || req.Repo == "" || freeForm {
					dir, err := resolveDir(cwdFlag)
					if err != nil {
						return err
					}
					if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
						return fmt.Errorf("--cwd %q is not an existing directory", dir)
					}
					req.Cwd = dir
				}
				// Join the same project `warden start` would: the explicit --project,
				// else the repository root of the launch directory.
				req.ProjectID, _ = cmd.Flags().GetString("project")
				if req.ProjectID == "" {
					base := req.Cwd
					if base == "" {
						base = req.Repo
					}
					var err error
					if req.ProjectID, err = projectIDForDir(base); err != nil {
						return err
					}
				}
				req.Prompt, _ = cmd.Flags().GetString("prompt")
				req.Agent, _ = cmd.Flags().GetString("agent")
				req.Branch, _ = cmd.Flags().GetString("branch")
			}
			sc, err := clientFor(cmd).ScheduleCreate(cmd.Context(), req)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if asJSON, _ := cmd.Flags().GetBool("json"); asJSON {
				return printJSON(out, sc)
			}
			fires := describeScheduleFire(sc, pipelineName, pipelineJobs)
			if now {
				fmt.Fprintf(out, "created schedule %s — fires %s — will fire within a minute\n", sc.ID, fires)
				return nil
			}
			fmt.Fprintf(out, "created schedule %s — fires %s — next run %s\n", sc.ID, fires, formatNextRun(sc.NextRun))
			return nil
		},
	}
	cmd.Flags().String("cron", "", "recurring cron spec, e.g. \"0 9 * * *\" (minute hour dom month dow); evaluated in the daemon host's local time unless prefixed TZ=<zone>")
	cmd.Flags().String("at", "", "single-shot time in the future, RFC3339 or 2006-01-02T15:04; a time without a zone is the local time of the machine running the daemon")
	cmd.Flags().Bool("now", false, "fire once as soon as possible (a single-shot due immediately); exclusive with --cron and --at")
	cmd.Flags().String("repo", "", "repo path: the agent runs in an isolated worktree off it (default role worker)")
	cmd.Flags().String("cwd", "", "directory to launch the agent in (default: the current directory unless --repo is given)")
	cmd.Flags().String("role", "", "agent role `<ROLE>`: "+roleChoices()+" (default: worker with --repo, otherwise general)")
	cmd.Flags().String("prompt", "", "the agent's initial prompt")
	cmd.Flags().String("agent", "", "optional name for the spawned agent")
	cmd.Flags().String("branch", "", "optional development branch / pr-review checkout")
	cmd.Flags().String("model", "", "model ID for the chosen AI CLI (requires --aicli). Empty lets the tier resolver pick an explicit model")
	cmd.Flags().String("aicli", "", "AI CLI `<ID>`: claude (default, stable) | aider | opencode | codex | crush | goose | cursor | antigravity — only claude is fully tested; codex/antigravity are beta, the rest experimental. See 'warden backend --help' for per-AI-CLI notes")
	cmd.Flags().String("ai-cli", "", "alias for --aicli")
	_ = cmd.Flags().MarkHidden("ai-cli")
	cmd.Flags().String("permission-mode", "", "permission mode: acceptEdits|auto|bypassPermissions|default|dontAsk|plan (default: from config or 'auto')")
	cmd.Flags().Bool("auto-restart", false, "auto-resume the agent if it crashes (errored), capped at a few attempts")
	cmd.Flags().String("tags", "", "comma-separated labels `<LIST>` stamped on every agent this schedule starts (e.g. --tags nightly,backend)")
	cmd.Flags().String("tier", "", "model tier for the quota-balanced resolver that picks the AI CLI+model: tier-1|tier-2|tier-3. An explicit --aicli/--model still wins")
	cmd.Flags().String("project", "", "`<ID>` of the daemon project the agent joins (its canonical path or remote URL, from 'warden projects list'). Empty = the git repository root of the launch directory")
	cmd.Flags().String("pipeline", "", "fire a pipeline from this YAML spec file (instead of an agent)")
	cmd.Flags().Bool("json", false, "print the created schedule as JSON")
	return cmd
}

// describeScheduleFire says what a schedule fires, for the create success line:
// "agent (role, ai-cli) in <dir>" or "pipeline <name> with N jobs". For a
// pipeline, name and jobs come from the spec the CLI just validated; fall back
// to a plain "pipeline" when they are unknown.
func describeScheduleFire(sc *schedule.Schedule, pipelineName string, jobs int) string {
	if sc.Mode == schedule.ModePipeline {
		if pipelineName == "" {
			return "pipeline"
		}
		noun := "jobs"
		if jobs == 1 {
			noun = "job"
		}
		return fmt.Sprintf("pipeline %s with %d %s", pipelineName, jobs, noun)
	}
	r := sc.Role
	if r == "" {
		if sc.Repo != "" {
			r = "worker"
		} else {
			r = "general"
		}
	}
	label := r
	if sc.AiCli != "" {
		label += ", " + sc.AiCli
	}
	dir := sc.Cwd
	if dir == "" {
		dir = sc.Repo
	}
	if dir == "" {
		return "agent (" + label + ")"
	}
	return "agent (" + label + ") in " + dir
}

func newScheduleListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List schedules",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			list, err := clientFor(cmd).ScheduleList(cmd.Context())
			if err != nil {
				return err
			}
			for _, sc := range list {
				spec := sc.Cron
				if sc.Kind == schedule.KindAt {
					spec = sc.At
				}
				state := "enabled"
				if !sc.Enabled {
					state = "inactive"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\t%q\t%s\tnext=%s",
					sc.ID, sc.Mode, sc.Kind, spec, state, formatNextRun(sc.NextRun))
				if sc.LastError != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "\tlast_error=%q", sc.LastError)
				}
				fmt.Fprintln(cmd.OutOrStdout())
			}
			return nil
		},
	}
}

func newScheduleGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <id>",
		Short: "Show one schedule, including its last-run outcome",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sc, err := clientFor(cmd).ScheduleGet(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			state := "enabled"
			if !sc.Enabled {
				state = "inactive"
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "%s\t%s\t%s\t%s\tnext=%s\tlast=%s\n",
				sc.ID, sc.Mode, sc.Kind, state, formatNextRun(sc.NextRun), formatNextRun(sc.LastRun))
			if sc.LastRunSessionID != "" {
				fmt.Fprintf(out, "last_run: %s (%s)\n", sc.LastRunSessionID, sc.LastRunStatus)
			}
			if sc.LastError != "" {
				fmt.Fprintf(out, "last_error: %s\n", sc.LastError)
			}
			return nil
		},
	}
}

func newScheduleEnableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "enable <id>",
		Short: "Enable a schedule so it fires again (re-arms next run)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sc, err := clientFor(cmd).ScheduleEnable(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if sc.NextRun != nil && !sc.NextRun.After(time.Now()) {
				fmt.Fprintf(cmd.OutOrStdout(), "enabled %s — will fire within a minute\n", sc.ID)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "enabled %s — next run %s\n", sc.ID, formatNextRun(sc.NextRun))
			return nil
		},
	}
}

func newScheduleDisableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "disable <id>",
		Short: "Disable a schedule so it stops firing (history preserved)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sc, err := clientFor(cmd).ScheduleDisable(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "disabled %s\n", sc.ID)
			return nil
		},
	}
}

func newScheduleDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete <id>",
		Short: "Delete a schedule",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := clientFor(cmd).ScheduleDelete(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "deleted %s\n", args[0])
			return nil
		},
	}
}

// formatNextRun renders a schedule's next-fire time (or "—" when inactive).
func formatNextRun(t *time.Time) string {
	if t == nil {
		return "—"
	}
	return t.Local().Format(time.RFC3339)
}
