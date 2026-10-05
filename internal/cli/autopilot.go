package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/srjn45/warden/internal/agentbackend"
	_ "github.com/srjn45/warden/internal/agentbackend/backends" // register adapters for binary detection
	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/client"
	"github.com/srjn45/warden/internal/config"
)

func newAutopilotCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "autopilot",
		Short: "Show autopilot status, scaffold adoption, and land worker branches",
		Long: "Autopilot is the unattended Plan execution mode. There is no per-repo switch:\n" +
			"start a run explicitly with `warden plan run <plan-id> --mode autopilot` and\n" +
			"control it with `warden plan pause|resume|stop`. This namespace shows status\n" +
			"(`status`), scaffolds adoption (`init`) and lands worker branches (`land`).\n" +
			"Configure the feature under the `autopilot` block in the config file.",
	}
	SetCommandHelpMetadata(cmd, "run", 30, "warden autopilot", "", NodeNamespace)

	children := []*cobra.Command{
		newAutopilotStatusCmd(),
		newAutopilotInitCmd(),
		newAutopilotLandCmd(),
	}
	for i, child := range children {
		SetCommandHelpMetadata(child, "run", (i+1)*10, "warden autopilot "+child.Name(), "", nodeKind(child))
		cmd.AddCommand(child)
	}
	// Hidden compatibility aliases for the retired run listing: all print the
	// same output as `autopilot status`.
	runAlias := newAutopilotRunListOnlyCmd()
	markStatusAlias(runAlias, "warden autopilot status")
	cmd.AddCommand(runAlias)
	// Deprecated one-release aliases: register / run lifecycle verbs translate via
	// the daemon to PlanID where safe (or return a precise migration error).
	for _, legacy := range []struct {
		factory   func() *cobra.Command
		canonical string
	}{
		{func() *cobra.Command { return newAutopilotEnableCmd("enable") }, "warden plan run"},
		{func() *cobra.Command { return newAutopilotEnableCmd("on") }, "warden plan run"},
		{func() *cobra.Command { return newAutopilotDisableCmd("disable") }, "warden plan pause"},
		{func() *cobra.Command { return newAutopilotDisableCmd("off") }, "warden plan pause"},
		{newAutopilotRegisterCmd, "warden plan run"},
		{newAutopilotListCmd, "warden autopilot status"},
		{func() *cobra.Command { return newAutopilotRunActionCmd("start") }, "warden plan run"},
		{func() *cobra.Command { return newAutopilotRunActionCmd("pause") }, "warden plan pause"},
		{func() *cobra.Command { return newAutopilotRunActionCmd("resume") }, "warden plan resume"},
		{func() *cobra.Command { return newAutopilotRunActionCmd("stop") }, "warden plan stop"},
		{func() *cobra.Command { return newAutopilotRunActionCmd("unregister") }, "warden plan stop"},
	} {
		alias := legacy.factory()
		markCompatibilityChild(alias, legacy.canonical)
		cmd.AddCommand(alias)
	}
	return cmd
}

func newAutopilotRunListOnlyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Compatibility alias for `autopilot status`",
		Long: "Hidden compatibility alias: prints the same output as `warden autopilot status`.\n" +
			"Lifecycle control (start/pause/resume/stop) moved to `warden plan run` /\n" +
			"`warden plan pause|resume|stop`.",
		Args: cobra.NoArgs,
		RunE: runAutopilotStatus,
	}
	addJSONFlag(cmd, "emit the raw autopilot status as JSON")
	list := newAutopilotListCmd()
	cmd.AddCommand(list)
	return cmd
}

// markStatusAlias hides cmd and its children as compatibility aliases of
// `autopilot status` (all share its canonical path).
func markStatusAlias(cmd *cobra.Command, canonicalPath string) {
	cmd.Hidden = true
	SetCommandHelpMetadata(cmd, "run", 900, canonicalPath, AliasCompatibility, nodeKind(cmd))
	for _, child := range cmd.Commands() {
		markStatusAlias(child, canonicalPath)
	}
}

func rewriteAutopilotHelpPaths(cmd *cobra.Command, legacyName, canonicalName string) {
	replacer := strings.NewReplacer(
		"warden autopilot "+legacyName, "warden autopilot "+canonicalName,
		"wd autopilot "+legacyName, "wd autopilot "+canonicalName,
		"`warden autopilot "+legacyName, "`warden autopilot "+canonicalName,
	)
	cmd.Long = replacer.Replace(cmd.Long)
	cmd.Example = replacer.Replace(cmd.Example)
}

func newAutopilotListCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "list", Short: "Compatibility alias for `autopilot status`", Args: cobra.NoArgs, RunE: runAutopilotStatus}
	addJSONFlag(cmd, "emit the raw autopilot status as JSON")
	return cmd
}

func newAutopilotRegisterCmd() *cobra.Command {
	var name, repo string
	cmd := &cobra.Command{Use: "register <plan-file>", Short: "Register a named autopilot plan", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		planFile, err := filepath.Abs(args[0])
		if err != nil {
			return fmt.Errorf("resolve plan file: %w", err)
		}
		if repo != "" {
			repo, err = filepath.Abs(repo)
			if err != nil {
				return fmt.Errorf("resolve repository: %w", err)
			}
		}
		r, err := clientFor(cmd).RegisterAutopilotRun(cmd.Context(), name, repo, planFile)
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "registered %s (%s)\n", r.Name, r.RunID)
		return nil
	}}
	cmd.Flags().StringVar(&name, "name", "", "unique run name within the repository")
	cmd.Flags().StringVar(&repo, "repo", "", "repository root (inferred from plan when omitted)")
	return cmd
}

func newAutopilotRunActionCmd(action string) *cobra.Command {
	return &cobra.Command{Use: action + " <run-id-or-name>", Short: action + " one autopilot run", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		id := args[0]
		if !strings.HasPrefix(id, "ap-") {
			runs, err := clientFor(cmd).ListAutopilotRuns(cmd.Context())
			if err != nil {
				return err
			}
			var matches []string
			for _, r := range runs {
				if r.Name == id {
					matches = append(matches, r.RunID)
				}
			}
			if len(matches) == 0 {
				return fmt.Errorf("autopilot run %q not found", id)
			}
			if len(matches) > 1 {
				return fmt.Errorf("autopilot run name %q is ambiguous across repositories; use a run id", id)
			}
			id = matches[0]
		}
		r, err := clientFor(cmd).ControlAutopilotRun(cmd.Context(), id, action)
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", r.RunID, r.Name, r.State)
		return nil
	}}
}

const autopilotEnableNotice = "note: the per-repo autopilot switch is gone; start runs with `wd plan run <plan-id> --mode autopilot`"

// newAutopilotEnableCmd is the DEPRECATED hidden no-op for enable/on.
func newAutopilotEnableCmd(name string) *cobra.Command {
	var repoFlag string
	cmd := &cobra.Command{
		Use:   name,
		Short: "Deprecated no-op (use `warden plan run --mode autopilot`)",
		Long: "Deprecated: there is no per-repo autopilot switch any more, so this does\n" +
			"nothing. Start a run with `warden plan run <plan-id> --mode autopilot`.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), autopilotEnableNotice)
			return nil
		},
	}
	cmd.Flags().StringVar(&repoFlag, "repo", "", "ignored (kept for compatibility)")
	return cmd
}

// newAutopilotDisableCmd is the DEPRECATED hidden disable/off: it pauses every
// active autopilot run in the target repo and says what it paused.
func newAutopilotDisableCmd(name string) *cobra.Command {
	var repoFlag string
	cmd := &cobra.Command{
		Use:   name,
		Short: "Deprecated: pause every active autopilot run in this repo (use `warden plan pause`)",
		Long: "Deprecated: there is no per-repo autopilot switch any more. This pauses every\n" +
			"active autopilot run in the repo (same as `warden plan pause` on each).\n" +
			"In-flight workers are left running. Use --repo to target another repository.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			repo, err := resolveAutopilotRepo(cmd, repoFlag)
			if err != nil {
				return err
			}
			c := clientFor(cmd)
			before, err := c.GetAutopilot(cmd.Context())
			if err != nil {
				return err
			}
			after, err := c.SetAutopilot(cmd.Context(), false, repo)
			if err != nil {
				return err
			}
			wasLive := map[string]bool{}
			for _, r := range before.Runs {
				if r.Repo == repo && (r.State == "active" || r.State == "healing" || r.State == "degraded") {
					wasLive[r.RunID] = true
				}
			}
			n := 0
			for _, r := range after.Runs {
				if wasLive[r.RunID] && r.State == "paused" {
					fmt.Fprintf(cmd.OutOrStdout(), "paused %s\t%s\n", r.RunID, r.Name)
					n++
				}
			}
			if n == 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "no active autopilot runs to pause in %s\n", repo)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "note: `autopilot disable` is deprecated; use `wd plan pause <plan-id>`")
			return nil
		},
	}
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repo root (default: the current git repository)")
	return cmd
}

// resolveAutopilotRepo resolves the repo root a per-repo toggle targets: the
// --repo override when given, else the current working directory, canonicalized to
// its git toplevel so it matches the repo autopilot resolves from a plan file.
func resolveAutopilotRepo(cmd *cobra.Command, override string) (string, error) {
	dir := override
	if dir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolve working directory: %w", err)
		}
		dir = cwd
	}
	root, err := autopilot.NewExecEnv().GitToplevel(cmd.Context(), dir)
	if err != nil {
		return "", fmt.Errorf("not inside a git repository (%s): %w", dir, err)
	}
	return root, nil
}

func newAutopilotStatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show autopilot status (every run)",
		Long: "Shows one line per run: run id, name,\n" +
			"state, plan id, repo, gate, integration branch, and backoff summary. Healing,\n" +
			"degraded and resting runs also print their next_step / resting_until, and runs\n" +
			"print the guardian's last diagnosis, per-task gate/fix state, resolver activity\n" +
			"and the final PR (all fields are also in --json). For a\n" +
			"running plan's task-level progress use `warden plan show`.",
		Args: cobra.NoArgs,
		RunE: runAutopilotStatus,
	}
	addJSONFlag(cmd, "emit the raw autopilot status as JSON")
	return cmd
}

func runAutopilotStatus(cmd *cobra.Command, _ []string) error {
	st, err := clientFor(cmd).GetAutopilot(cmd.Context())
	if err != nil {
		return err
	}
	if jsonRequested(cmd) {
		return printJSON(cmd.OutOrStdout(), st)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "autopilot: %d run(s)\n", len(st.Runs))
	printAutopilotRuns(cmd, st)
	return nil
}

// printAutopilotRuns renders one line per run: id, name, state, plan id, repo,
// gate, integration branch, backoff summary; plus next_step / resting_until when set.
func printAutopilotRuns(cmd *cobra.Command, st client.AutopilotStatus) {
	for _, r := range st.Runs {
		fmt.Fprintf(cmd.OutOrStdout(), "  %s\t%s\t%s\tplan=%s\t%s\tgate=%s\tbranch=%s\tbackoff=%s\n",
			r.RunID, r.Name, r.State, dash(r.PlanID), r.Repo, dash(r.Gate), dash(r.IntegrationBranch), backoffSummary(r.Backoff))
		if r.LastProgressAt != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "    last progress: %s (watchdog: %s)\n", r.LastProgressAt, dash(r.Watchdog))
		}
		if r.NextStep != nil {
			line := "    next: " + r.NextStep.Action
			if r.NextStep.At != "" {
				line += " at " + r.NextStep.At
			}
			if r.NextStep.Owner != "" {
				line += " (" + r.NextStep.Owner + ")"
			}
			fmt.Fprintln(cmd.OutOrStdout(), line)
		}
		if r.RestingUntil != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "    resting until: %s\n", r.RestingUntil)
		}
		for _, l := range r.SurfaceLines() {
			fmt.Fprintln(cmd.OutOrStdout(), "    "+l)
		}
		if r.NeedsAttention != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "    NEEDS ATTENTION: %s\n", r.NeedsAttention)
		}
	}
}

func backoffSummary(b *client.AutopilotBackoff) string {
	if b == nil {
		return "-"
	}
	out := fmt.Sprintf("stage %d", b.Stage)
	if b.Kind != "" {
		out += " kind " + b.Kind
	}
	if b.NextRetryAt != "" {
		out += " retry " + b.NextRetryAt
	}
	if b.LastError != "" {
		out += " (" + b.LastError + ")"
	}
	return out
}

func newAutopilotInitCmd() *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Scaffold the plan file and integration branch for autopilot",
		Long: "Creates a named template under plans/ in the current git repository (if absent),\n" +
			"creates the integration branch off the default branch if absent, and prints a\n" +
			"CI-coverage hint when no workflow covers integration pull requests. Nothing is\n" +
			"registered with the daemon. Next, edit the plan file, create the canonical plan\n" +
			"with `wd plan create`, then start it with `wd plan run <id> --mode autopilot`.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			env := autopilot.NewExecEnv()
			cwd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("resolve working directory: %w", err)
			}
			repo, err := env.GitToplevel(cmd.Context(), cwd)
			if err != nil {
				return fmt.Errorf("not inside a git repository: %w", err)
			}
			cfg := config.Load(configPathFor(cmd))
			return autopilot.Init(cmd.Context(), env, repo, autopilot.InitConfig{
				Name:              name,
				IntegrationBranch: cfg.AutopilotIntegrationBranch(),
			}, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&name, "name", "default", "plan name (creates plans/<name>.yaml)")
	return cmd
}

// newAutopilotLandCmd is the canonical `warden autopilot land` command.
func newAutopilotLandCmd() *cobra.Command {
	cmd := newLandCmd()
	rewriteAutopilotHelpPaths(cmd, "land", "land")
	return cmd
}

// newLandCmd is the legacy top-level `warden land` compatibility wrapper.
// merge of one autopilot worker branch into the integration branch (autopilot.md
// §6). It mirrors the MCP `land` tool the brain uses.
func newLandCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "land <agent-or-branch>",
		Short: "Land an autopilot worker branch into the integration branch",
		Long: "Merges one autopilot worker branch into the integration branch — the brain's\n" +
			"only merge path. Runs every precondition (owning run active, branch\n" +
			"autopilot-owned, a PR based on the integration branch, the resolved gate green\n" +
			"for the PR head, and the PR mergeable), merges with the configured strategy,\n" +
			"deletes the worker branch if configured, and records the landing. Idempotent:\n" +
			"re-issuing after a merge reports already-landed with no second merge. On a\n" +
			"precondition failure it prints the typed kind\n" +
			"(gate_pending|gate_red|ci_missing|not_mergeable|not_owned|run_disabled|wrong_base).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := clientFor(cmd).Land(cmd.Context(), args[0])
			if err != nil {
				var le *client.AutopilotLandError
				if errors.As(err, &le) {
					fmt.Fprintf(cmd.ErrOrStderr(), "land failed: %s\n", le.Kind)
					if le.Detail != "" {
						fmt.Fprintf(cmd.ErrOrStderr(), "  %s\n", le.Detail)
					}
					return fmt.Errorf("not landed (%s)", le.Kind)
				}
				return err
			}
			if jsonRequested(cmd) {
				return printJSON(cmd.OutOrStdout(), res)
			}
			if res.AlreadyLanded {
				fmt.Fprintf(cmd.OutOrStdout(), "already landed: %s @ %s (PR #%d)\n", res.Branch, res.SHA, res.PR)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "landed %s @ %s (PR #%d)\n", res.Branch, res.SHA, res.PR)
			return nil
		},
	}
	addJSONFlag(cmd, "emit the raw land result as JSON")
	return cmd
}

// detectInstalledBackends returns the ids of every registered backend whose
// binary is found on PATH. It is a thin filter over the shared
// agentbackend.Detect() sweep (the one detector for the whole product; see
// docs/specs/2026-08-06-backend-registry.md §4).
func detectInstalledBackends() []string {
	det := agentbackend.Detect()
	found := make([]string, 0, len(det))
	for _, d := range det {
		if d.Installed {
			found = append(found, d.ID)
		}
	}
	sort.Strings(found)
	return found
}
