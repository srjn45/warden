package cli

import (
	"errors"
	"fmt"
	"os"
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
		Short: "Turn autopilot mode on/off per repo and show its status",
		Long: "Autopilot runs a long-lived headless brain agent per plan that decomposes a\n" +
			"goal, spawns workers, and lands green work into an integration branch\n" +
			"unattended. The switch is PER-REPO: `warden autopilot on` run inside a repo\n" +
			"enables only that repo (others are unaffected), and the enabled set is persisted\n" +
			"so repos come back up across a daemon restart. The plan/manager/merge template\n" +
			"stays global in the `autopilot` config block. Enabling runs a preflight (plan\n" +
			"file valid, gh authenticated, integration branch present, at most one active run\n" +
			"per repo) and fails fast with the full list of problems so you fix everything in\n" +
			"one pass. `off` is the kill switch. Configure the feature under the `autopilot`\n" +
			"block in the config file (or scaffold it with `warden autopilot init`).",
	}
	cmd.AddCommand(newAutopilotOnCmd(), newAutopilotOffCmd(), newAutopilotStatusCmd(), newAutopilotInitCmd(),
		newAutopilotRegisterCmd(), newAutopilotRunActionCmd("start"), newAutopilotRunActionCmd("pause"),
		newAutopilotRunActionCmd("resume"), newAutopilotRunActionCmd("stop"))
	return cmd
}

func newAutopilotRegisterCmd() *cobra.Command {
	var name, repo string
	cmd := &cobra.Command{Use: "register <plan-file>", Short: "Register a named autopilot plan", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		r, err := clientFor(cmd).RegisterAutopilotRun(cmd.Context(), name, repo, args[0])
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

func newAutopilotOnCmd() *cobra.Command {
	var repoFlag string
	cmd := &cobra.Command{
		Use:   "on",
		Short: "Enable autopilot for this repo (runs the enable-time preflight)",
		Long: "Enables autopilot for the current git repository only (other repos are\n" +
			"unaffected). Runs the enable-time preflight and, on success, persists the repo\n" +
			"as enabled so it comes back up across a daemon restart. Use --repo to target a\n" +
			"different repository.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			repo, err := resolveAutopilotRepo(cmd, repoFlag)
			if err != nil {
				return err
			}
			st, err := clientFor(cmd).SetAutopilot(cmd.Context(), true, repo)
			if err != nil {
				var pfe *client.AutopilotPreflightError
				if errors.As(err, &pfe) {
					fmt.Fprintln(cmd.ErrOrStderr(), "autopilot enable-time preflight failed — fix these and retry:")
					for _, f := range pfe.Failures {
						fmt.Fprintf(cmd.ErrOrStderr(), "  • %s\n", f)
					}
					fmt.Fprintln(cmd.ErrOrStderr(), "\nhint: run `warden autopilot init` to scaffold a plan file and config block")
					return fmt.Errorf("autopilot not enabled (%d preflight failure(s))", len(pfe.Failures))
				}
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "autopilot enabled for %s — %d run(s)\n", repo, len(st.Runs))
			printAutopilotRuns(cmd, st)
			return nil
		},
	}
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repo root to enable (default: the current git repository)")
	return cmd
}

func newAutopilotOffCmd() *cobra.Command {
	var repoFlag string
	cmd := &cobra.Command{
		Use:   "off",
		Short: "Disable autopilot for this repo (kill switch — stops spawning/landing)",
		Long: "Disables autopilot for the current git repository only (other enabled repos\n" +
			"keep running). In-flight workers are left running. Use --repo to target a\n" +
			"different repository.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			repo, err := resolveAutopilotRepo(cmd, repoFlag)
			if err != nil {
				return err
			}
			if _, err := clientFor(cmd).SetAutopilot(cmd.Context(), false, repo); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "autopilot disabled for %s\n", repo)
			return nil
		},
	}
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repo root to disable (default: the current git repository)")
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
	return &cobra.Command{
		Use:   "status",
		Short: "Show autopilot status (which repos are enabled, and each run)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := clientFor(cmd).GetAutopilot(cmd.Context())
			if err != nil {
				return err
			}
			state := "disabled"
			if st.Enabled {
				state = "enabled"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "autopilot: %s — %d repo(s), %d run(s)\n",
				state, len(st.EnabledRepos), len(st.Runs))
			for _, repo := range st.EnabledRepos {
				fmt.Fprintf(cmd.OutOrStdout(), "  enabled: %s\n", repo)
			}
			printAutopilotRuns(cmd, st)
			return nil
		},
	}
}

// printAutopilotRuns renders one line per run: id, state, gate, plan file, repo.
func printAutopilotRuns(cmd *cobra.Command, st client.AutopilotStatus) {
	for _, r := range st.Runs {
		fmt.Fprintf(cmd.OutOrStdout(), "  %s\t%s\tgate=%s\t%s\t%s\n",
			r.RunID, r.State, r.Gate, r.PlanFile, r.Repo)
	}
}

func newAutopilotInitCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Scaffold autopilot adoption in the current repo",
		Long: "Creates a template autopilot.plan.yaml in the current git repository (if absent),\n" +
			"updates the autopilot block in the warden config with the plan file and detected\n" +
			"backends (assign them to cost tiers before enabling), creates the integration branch\n" +
			"off the default branch if absent, and prints a CI-coverage hint when no workflow\n" +
			"covers integration pull requests. After init, edit the plan file and run\n" +
			"`warden autopilot on` to enable.",
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
			cfgPath := configPathFor(cmd)
			cfg := config.Load(cfgPath)
			return autopilot.Init(cmd.Context(), env, repo, autopilot.InitConfig{
				ConfigPath:        cfgPath,
				PlanFile:          "autopilot.plan.yaml",
				IntegrationBranch: cfg.AutopilotIntegrationBranch(),
				Backends:          detectInstalledBackends(),
			}, cmd.OutOrStdout())
		},
	}
}

// newLandCmd is the top-level `warden land` command: the guarded, idempotent
// merge of one autopilot worker branch into the integration branch (autopilot.md
// §6). It mirrors the MCP `land` tool the brain uses.
func newLandCmd() *cobra.Command {
	return &cobra.Command{
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
			if res.AlreadyLanded {
				fmt.Fprintf(cmd.OutOrStdout(), "already landed: %s @ %s (PR #%d)\n", res.Branch, res.SHA, res.PR)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "landed %s @ %s (PR #%d)\n", res.Branch, res.SHA, res.PR)
			return nil
		},
	}
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
