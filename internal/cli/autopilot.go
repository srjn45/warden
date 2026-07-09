package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"

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
		Short: "Turn autopilot mode on/off and show its status",
		Long: "Autopilot runs a long-lived headless brain agent per plan that decomposes a\n" +
			"goal, spawns workers, and lands green work into an integration branch\n" +
			"unattended. Enabling runs a preflight (plan file valid, gh authenticated,\n" +
			"integration branch present, at most one active run per repo) and fails fast\n" +
			"with the full list of problems so you fix everything in one pass. `off` is the\n" +
			"kill switch. Configure the feature under the `autopilot` block in the config\n" +
			"file (or scaffold it with `warden autopilot init`).",
	}
	cmd.AddCommand(newAutopilotOnCmd(), newAutopilotOffCmd(), newAutopilotStatusCmd(), newAutopilotInitCmd())
	return cmd
}

func newAutopilotOnCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "on",
		Short: "Enable autopilot (runs the enable-time preflight)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := clientFor(cmd).SetAutopilot(cmd.Context(), true)
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
			fmt.Fprintf(cmd.OutOrStdout(), "autopilot enabled — %d run(s)\n", len(st.Runs))
			printAutopilotRuns(cmd, st)
			return nil
		},
	}
}

func newAutopilotOffCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "off",
		Short: "Disable autopilot (kill switch — stops spawning/landing)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := clientFor(cmd).SetAutopilot(cmd.Context(), false); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "autopilot disabled")
			return nil
		},
	}
}

func newAutopilotStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show autopilot status",
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
			fmt.Fprintf(cmd.OutOrStdout(), "autopilot: %s — %d run(s)\n", state, len(st.Runs))
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

// detectInstalledBackends returns the ids of every registered backend whose
// binary is found on PATH.
func detectInstalledBackends() []string {
	var found []string
	for _, id := range agentbackend.IDs() {
		b, err := agentbackend.Get(id)
		if err != nil {
			continue
		}
		if _, err := exec.LookPath(b.Binary()); err == nil {
			found = append(found, id)
		}
	}
	sort.Strings(found)
	return found
}
