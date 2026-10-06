package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
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
		newScheduleEditCmd(), newScheduleRunCmd(), newScheduleEnableCmd(), newScheduleDisableCmd(), newScheduleDeleteCmd(),
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
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List schedules",
		Long: "List schedules as a table. Columns: NAME, STATE (enabled, disabled, done for a\n" +
			"single-shot that has fired, failed for a single-shot whose fire failed), WHEN\n" +
			"(the cron spec, or the single-shot time), FIRES (the agent and its role, or\n" +
			"the pipeline), NEXT (the next run, local time) and LAST (when it last ran and\n" +
			"how it went). A schedule whose last run failed has the error on the line\n" +
			"beneath it; a recurring one is still enabled and will try again.\n\n" +
			"--json prints the raw result.",
		Example: "  warden schedule list\n" +
			"  warden schedule ls --json",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			list, err := clientFor(cmd).ScheduleList(cmd.Context())
			if err != nil {
				return wrapScheduleError(err)
			}
			out := cmd.OutOrStdout()
			if asJSON, _ := cmd.Flags().GetBool("json"); asJSON {
				if list == nil {
					list = []*schedule.Schedule{}
				}
				return printJSON(out, list)
			}
			if len(list) == 0 {
				fmt.Fprintln(out, "no schedules")
				fmt.Fprintln(out, "hint: wd schedule create nightly --cron \"0 2 * * *\" --prompt \"run the nightly cleanup\"")
				return nil
			}
			return printScheduleTable(out, list, time.Now())
		},
	}
	cmd.Flags().Bool("json", false, "output as JSON")
	return cmd
}

// wrapScheduleError turns the daemon's scheduler-disabled answer into the
// instruction to enable it.
func wrapScheduleError(err error) error {
	var se *client.StatusError
	if errors.As(err, &se) && strings.Contains(se.Msg, "scheduler disabled") {
		return fmt.Errorf("the scheduler is turned off: set scheduler_enabled: true in the warden config file (see `warden config path`) and restart the daemon")
	}
	return err
}

// scheduleWhen is the timing of a schedule: its cron spec or its single-shot time.
func scheduleWhen(sc *schedule.Schedule) string {
	if sc.Kind == schedule.KindAt {
		if t, err := schedule.ParseAt(sc.At); err == nil {
			return t.Local().Format("2006-01-02 15:04")
		}
		return sc.At
	}
	return sc.Cron
}

// scheduleFires is what a schedule fires, short enough for a table cell.
func scheduleFires(sc *schedule.Schedule) string {
	if sc.Mode == schedule.ModePipeline {
		if p, err := pipeline.ParseSpec([]byte(sc.Spec)); err == nil && p.Name != "" {
			return "pipeline " + p.Name
		}
		return "pipeline"
	}
	return "agent (" + scheduleRole(sc) + ")"
}

func scheduleRole(sc *schedule.Schedule) string {
	switch {
	case sc.Role != "":
		return sc.Role
	case sc.Repo != "":
		return "worker"
	}
	return "general"
}

// relativeTime renders t relative to now: "in 3h", "2d ago", "just now".
func relativeTime(t, now time.Time) string {
	d := t.Sub(now)
	future := d > 0
	if d < 0 {
		d = -d
	}
	var v string
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		v = fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 48*time.Hour:
		v = fmt.Sprintf("%dh", int(d/time.Hour))
	default:
		v = fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	}
	if future {
		return "in " + v
	}
	return v + " ago"
}

func scheduleNext(sc *schedule.Schedule, now time.Time) string {
	if sc.NextRun == nil {
		return "-"
	}
	return sc.NextRun.Local().Format("2006-01-02 15:04") + " (" + relativeTime(*sc.NextRun, now) + ")"
}

// scheduleLastOutcome is the outcome word of the last run: its live status, or
// "failed" when the fire itself errored.
func scheduleLastOutcome(sc *schedule.Schedule) string {
	switch {
	case sc.LastError != "":
		return "failed"
	case sc.LastRunStatus != "":
		return sc.LastRunStatus
	}
	return "fired"
}

func scheduleLast(sc *schedule.Schedule, now time.Time) string {
	if sc.LastRun == nil {
		return "-"
	}
	return sc.LastRun.Local().Format("2006-01-02 15:04") + " " + scheduleLastOutcome(sc)
}

func printScheduleTable(w io.Writer, list []*schedule.Schedule, now time.Time) error {
	// Flush the table around each error line so the long text does not stretch
	// the columns of the rows above and below it.
	var buf bytes.Buffer
	tw := tabwriter.NewWriter(&buf, 0, 2, 2, ' ', 0)
	flush := func() error {
		if err := tw.Flush(); err != nil {
			return err
		}
		for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
			if line != "" {
				fmt.Fprintln(w, strings.TrimRight(line, " "))
			}
		}
		buf.Reset()
		return nil
	}
	rows := [][]string{{"NAME", "STATE", "WHEN", "FIRES", "NEXT", "LAST"}}
	for _, sc := range list {
		rows = append(rows, []string{sc.Name, string(sc.State()), scheduleWhen(sc),
			scheduleFires(sc), scheduleNext(sc, now), scheduleLast(sc, now)})
	}
	// Align every column over all rows, then print the error lines in between.
	widths := make([]int, 6)
	for _, r := range rows {
		for i, c := range r {
			if n := len([]rune(c)); n > widths[i] {
				widths[i] = n
			}
		}
	}
	_ = flush
	for i, r := range rows {
		var cells []string
		for j, c := range r {
			cells = append(cells, c+strings.Repeat(" ", widths[j]-len([]rune(c))))
		}
		fmt.Fprintln(w, strings.TrimRight(strings.Join(cells, "  "), " "))
		if i > 0 && list[i-1].LastError != "" {
			fmt.Fprintf(w, "  error: %s\n", list[i-1].LastError)
		}
	}
	return nil
}

func newScheduleShowCmd() *cobra.Command {
	cmd := newScheduleGetCmd()
	cmd.Use = "show <id>"
	cmd.Aliases = nil
	return cmd
}

func newScheduleGetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get <id>",
		Short: "Show one schedule, including its last-run outcome",
		Long: "Show a schedule: its state, timing, next run and when it was created, then\n" +
			"what it fires in full (for an agent: the prompt, directory, repo, branch, role,\n" +
			"model, AI CLI and agent name; for a pipeline: its name and job count) and its\n" +
			"last run with the command to look at it. --spec also prints the stored\n" +
			"pipeline YAML. --json prints the raw record.",
		Example: "  warden schedule show nightly\n" +
			"  warden schedule show nightly --spec\n" +
			"  warden schedule show nightly --json",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sc, err := clientFor(cmd).ScheduleGet(cmd.Context(), args[0])
			if err != nil {
				err = wrapScheduleError(err)
				var se *client.StatusError
				if errors.As(err, &se) && se.Code == http.StatusNotFound {
					return fmt.Errorf("%w\nNext: wd schedule list", err)
				}
				return err
			}
			out := cmd.OutOrStdout()
			if asJSON, _ := cmd.Flags().GetBool("json"); asJSON {
				return printJSON(out, sc)
			}
			withSpec, _ := cmd.Flags().GetBool("spec")
			fmt.Fprint(out, renderScheduleDetail(sc, time.Now(), withSpec))
			return nil
		},
	}
	cmd.Flags().Bool("json", false, "output as JSON")
	cmd.Flags().Bool("spec", false, "also print the stored pipeline YAML")
	return cmd
}

// describeCron is a plain reading of a cron spec when it is cheap to give one.
func describeCron(spec string) string {
	switch strings.TrimSpace(spec) {
	case "@hourly":
		return "every hour"
	case "@daily", "@midnight":
		return "every day at 00:00"
	case "@weekly":
		return "every week, Sunday 00:00"
	case "@monthly":
		return "on the 1st of every month at 00:00"
	case "@yearly", "@annually":
		return "every year on 1 January at 00:00"
	}
	f := strings.Fields(spec)
	if len(f) == 5 && f[2] == "*" && f[3] == "*" && f[4] == "*" {
		min, hr := f[0], f[1]
		if _, err := strconv.Atoi(min); err == nil {
			if _, err := strconv.Atoi(hr); err == nil {
				return fmt.Sprintf("every day at %02s:%02s", hr, min)
			}
		}
	}
	return ""
}

func renderScheduleDetail(sc *schedule.Schedule, now time.Time, withSpec bool) string {
	var b strings.Builder
	kv := func(k, v string) { fmt.Fprintf(&b, "%-12s%s\n", k+":", v) }
	kv("name", sc.Name)
	state := string(sc.State())
	if sc.State() == schedule.StateEnabled && sc.LastError != "" {
		state += " (last run failed)"
	}
	kv("state", state)
	if sc.Kind == schedule.KindAt {
		kv("when", "once, at "+scheduleWhen(sc))
	} else {
		w := "cron " + sc.Cron
		if d := describeCron(sc.Cron); d != "" {
			w += " — " + d
		}
		kv("when", w)
	}
	kv("next run", scheduleNext(sc, now))
	kv("created", sc.CreatedAt.Local().Format("2006-01-02 15:04"))

	b.WriteString("\n")
	if sc.Mode == schedule.ModePipeline {
		kv("fires", "pipeline")
		name, jobs := "", 0
		if p, err := pipeline.ParseSpec([]byte(sc.Spec)); err == nil {
			name, jobs = p.Name, userJobs(p)
		}
		kv("pipeline", dashIfEmpty(name))
		kv("jobs", strconv.Itoa(jobs))
		if withSpec {
			b.WriteString("\nspec:\n")
			b.WriteString(strings.TrimRight(sc.Spec, "\n"))
			b.WriteString("\n")
		}
	} else {
		kv("fires", "agent")
		kv("prompt", dashIfEmpty(sc.Prompt))
		kv("directory", dashIfEmpty(sc.Cwd))
		kv("repo", dashIfEmpty(sc.Repo))
		kv("branch", dashIfEmpty(sc.Branch))
		kv("role", scheduleRole(sc))
		kv("model", dashIfEmpty(sc.Model))
		kv("ai cli", dashIfEmpty(sc.AiCli))
		kv("agent name", dashIfEmpty(sc.Agent))
	}

	b.WriteString("\n")
	if sc.LastRun == nil {
		kv("last run", "never")
		return b.String()
	}
	kv("last run", sc.LastRun.Local().Format("2006-01-02 15:04")+" ("+relativeTime(*sc.LastRun, now)+")")
	if sc.LastRunSessionID != "" {
		kv("produced", sc.LastRunSessionID)
	}
	kv("status", scheduleLastOutcome(sc))
	if sc.LastError != "" {
		kv("error", sc.LastError)
	}
	if sc.LastRunSessionID != "" {
		cmdHint := "wd status " + sc.LastRunSessionID
		if sc.Mode == schedule.ModePipeline {
			cmdHint = "wd pipeline show " + sc.LastRunSessionID
		}
		kv("look at it", cmdHint)
	}
	return b.String()
}

// newScheduleEditCmd changes only flags explicitly supplied by the caller. This
// is important for optional fields: --prompt "" deliberately clears it.
func newScheduleEditCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "edit <id> [--cron <spec> | --at <time>] [agent flags] | --pipeline <spec.yaml>",
		Short: "Edit a schedule's timing or fire payload",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.Flags().Changed("cron") && cmd.Flags().Changed("at") {
				return fmt.Errorf("provide exactly one of --cron or --at, not both")
			}
			req := client.ScheduleUpdateRequest{}
			set := func(name string, dst **string) error {
				if !cmd.Flags().Changed(name) {
					return nil
				}
				v, err := cmd.Flags().GetString(name)
				if err != nil {
					return err
				}
				*dst = &v
				return nil
			}
			for _, field := range []struct {
				name string
				dst  **string
			}{
				{"cron", &req.Cron}, {"at", &req.At}, {"repo", &req.Repo}, {"cwd", &req.Cwd},
				{"role", &req.Role}, {"prompt", &req.Prompt}, {"agent", &req.Agent}, {"branch", &req.Branch},
				{"model", &req.Model}, {"aicli", &req.AiCli},
			} {
				if err := set(field.name, field.dst); err != nil {
					return err
				}
			}
			if cmd.Flags().Changed("ai-cli") {
				v, _ := cmd.Flags().GetString("ai-cli")
				req.AiCli = &v
			}
			if cmd.Flags().Changed("pipeline") {
				path, _ := cmd.Flags().GetString("pipeline")
				data, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				if _, err := pipeline.ParseSpec(data); err != nil {
					return fmt.Errorf("invalid pipeline spec %s: %w", path, err)
				}
				v := string(data)
				req.Spec = &v
			}
			if req.Cron == nil && req.At == nil && req.Repo == nil && req.Cwd == nil && req.Role == nil && req.Prompt == nil && req.Agent == nil && req.Branch == nil && req.Model == nil && req.AiCli == nil && req.Spec == nil {
				return fmt.Errorf("nothing to change: pass an edit flag")
			}
			sc, err := clientFor(cmd).ScheduleUpdate(cmd.Context(), args[0], req)
			if err != nil {
				return wrapScheduleError(err)
			}
			if asJSON, _ := cmd.Flags().GetBool("json"); asJSON {
				return printJSON(cmd.OutOrStdout(), sc)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "updated %s — next run %s\n", sc.ID, formatNextRun(sc.NextRun))
			return nil
		},
	}
	cmd.Flags().String("cron", "", "new recurring cron spec (switches from --at)")
	cmd.Flags().String("at", "", "new single-shot time in the future (switches from --cron)")
	cmd.Flags().String("repo", "", "repo path for an agent schedule")
	cmd.Flags().String("cwd", "", "agent launch directory (empty clears it)")
	cmd.Flags().String("role", "", "agent role (empty clears it)")
	cmd.Flags().String("prompt", "", "agent prompt (empty clears it)")
	cmd.Flags().String("agent", "", "agent name (empty clears it)")
	cmd.Flags().String("branch", "", "development branch (empty clears it)")
	cmd.Flags().String("model", "", "model ID (empty clears it)")
	cmd.Flags().String("aicli", "", "AI CLI (empty clears it)")
	cmd.Flags().String("ai-cli", "", "alias for --aicli")
	_ = cmd.Flags().MarkHidden("ai-cli")
	cmd.Flags().String("pipeline", "", "new pipeline YAML spec file")
	cmd.Flags().Bool("json", false, "print the updated schedule as JSON")
	return cmd
}

func newScheduleRunCmd() *cobra.Command {
	return &cobra.Command{
		Use: "run <id>", Short: "Fire a schedule once now without changing its next run", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sc, runID, err := clientFor(cmd).ScheduleRun(cmd.Context(), args[0])
			if err != nil {
				return wrapScheduleError(err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "ran %s — started %s — next run %s\n", sc.ID, runID, formatNextRun(sc.NextRun))
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
