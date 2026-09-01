package cli

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/srjn45/warden/internal/client"
	"github.com/srjn45/warden/internal/schedule"
)

func newScheduleCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "schedule",
		Short: "Schedule recurring (--cron) or single-shot (--at) agents and pipelines",
		Long: "Create timer-driven triggers that the daemon fires on a schedule: a recurring\n" +
			"cron spec (--cron \"0 9 * * *\") or a single-shot time (--at 2026-06-27T09:00).\n" +
			"Each schedule fires either one agent spawn (the default — pass --type/--repo/\n" +
			"--prompt) or a pipeline (--pipeline <spec.yaml>). The scheduler is opt-in: set\n" +
			"scheduler_enabled: true in the config file and keep the daemon running.",
	}
	SetCommandHelpMetadata(cmd, "run", 40, "warden schedule", "", NodeNamespace)
	children := []*cobra.Command{
		newScheduleCreateCmd(), newScheduleListCmd(), newScheduleGetCmd(),
		newScheduleEnableCmd(), newScheduleDisableCmd(), newScheduleDeleteCmd(),
	}
	for i, child := range children {
		SetCommandHelpMetadata(child, "run", (i+1)*10, "warden schedule "+child.Name(), "", NodeLeaf)
		cmd.AddCommand(child)
	}
	return cmd
}

func newScheduleCreateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create <name> (--cron <spec> | --at <time>) [--type <t> --repo <p> --prompt <s> | --pipeline <spec.yaml>]",
		Short: "Create a schedule that fires an agent or a pipeline",
		Long: "Create a recurring (--cron) or single-shot (--at) schedule. By default a\n" +
			"schedule fires one agent spawn — pass --type, --repo, --prompt (and optionally\n" +
			"--agent name / --branch). Pass --pipeline <spec.yaml> instead to fire a pipeline\n" +
			"(its name is timestamp-suffixed per fire so recurring runs don't collide).\n" +
			"Provide exactly one of --cron/--at and exactly one fire mode.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cron, _ := cmd.Flags().GetString("cron")
			at, _ := cmd.Flags().GetString("at")
			pipelineFile, _ := cmd.Flags().GetString("pipeline")
			req := client.ScheduleCreateRequest{
				Name: args[0],
				Cron: cron,
				At:   at,
			}
			if pipelineFile != "" {
				data, err := os.ReadFile(pipelineFile)
				if err != nil {
					return err
				}
				req.Spec = string(data)
			} else {
				req.Type, _ = cmd.Flags().GetString("type")
				req.Repo, _ = cmd.Flags().GetString("repo")
				req.Prompt, _ = cmd.Flags().GetString("prompt")
				req.Agent, _ = cmd.Flags().GetString("agent")
				req.Branch, _ = cmd.Flags().GetString("branch")
			}
			sc, err := clientFor(cmd).ScheduleCreate(cmd.Context(), req)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "created schedule %s (%s %s) — next run %s\n",
				sc.ID, sc.Kind, sc.Mode, formatNextRun(sc.NextRun))
			return nil
		},
	}
	cmd.Flags().String("cron", "", "recurring cron spec, e.g. \"0 9 * * *\" (minute hour dom month dow)")
	cmd.Flags().String("at", "", "single-shot time, RFC3339 or 2006-01-02T15:04 (local)")
	cmd.Flags().String("type", "", "agent task type (e.g. pr-review, development); empty = free-form")
	cmd.Flags().String("repo", "", "repo path (required for a typed agent)")
	cmd.Flags().String("prompt", "", "the agent's initial prompt")
	cmd.Flags().String("agent", "", "optional name for the spawned agent")
	cmd.Flags().String("branch", "", "optional development branch / pr-review checkout")
	cmd.Flags().String("pipeline", "", "fire a pipeline from this YAML spec file (instead of an agent)")
	return cmd
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
