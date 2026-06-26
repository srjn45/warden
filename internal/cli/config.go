package cli

import (
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"github.com/srjn45/warden/internal/config"
)

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Show the resolved configuration (and its file path)",
		Long: "Print the live, resolved configuration values warden is using, grouped by area,\n" +
			"with the config file path at the top. Edit the file by hand to change settings.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			path := configPathFor(cmd)
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "config file: %s\n", path)
			if _, err := os.Stat(path); os.IsNotExist(err) {
				fmt.Fprintln(out, "(file not found — showing defaults; run `warden config init` to create it)")
			}
			fmt.Fprintln(out)
			printConfig(out, config.Load(path))
			return nil
		},
	}
	cmd.AddCommand(newConfigPathCmd(), newConfigInitCmd())
	return cmd
}

func newConfigPathCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "path",
		Short: "Print the resolved config file path",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), configPathFor(cmd))
			return nil
		},
	}
}

func newConfigInitCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Create the config file (or migrate it, adding any missing keys)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			path := configPathFor(cmd)
			if err := config.Reconcile(path); err != nil {
				return fmt.Errorf("config init: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "config ready: %s\n", path)
			return nil
		},
	}
}

// printConfig renders cfg grouped by area as aligned key: value rows.
func printConfig(out io.Writer, cfg config.Config) {
	groups := []struct {
		title string
		rows  [][2]string
	}{
		{"daemon", [][2]string{
			{"addr", cfg.Addr},
			{"data_dir", cfg.DataDir},
			{"claude_projects_dir", cfg.ClaudeProjectsDir},
			{"allow_nonloopback", fmt.Sprintf("%t", cfg.AllowNonLoopback)},
		}},
		{"agents", [][2]string{
			{"default_permission_mode", cfg.DefaultPermissionMode},
			{"model_default", cfg.ModelDefault},
			{"auto_approve", fmt.Sprintf("%t", cfg.AutoApprove.Enabled)},
			{"pipeline_hint", fmt.Sprintf("%t", cfg.PipelineHint)},
			{"pipeline_keep_done", fmt.Sprintf("%t", cfg.PipelineKeepDone)},
			{"spawn_gate", fmt.Sprintf("%t", cfg.SpawnGateEnabled)},
			{"spawn_gate_max_agents", fmt.Sprintf("%d", cfg.SpawnGateMaxAgents)},
			{"tutorial", fmt.Sprintf("%t", cfg.Tutorial)},
		}},
		{"notifications & metrics", [][2]string{
			{"approvals", fmt.Sprintf("%t", cfg.ApprovalsEnabled)},
			{"notify", fmt.Sprintf("%t", cfg.NotifyEnabled)},
			{"webhook_enabled", fmt.Sprintf("%t", cfg.WebhookEnabled)},
			// The URL carries a secret (e.g. a Slack token), so show set/unset
			// rather than the value to keep it out of terminals and screenshots.
			{"webhook_url", webhookURLDisplay(cfg.WebhookURL)},
			{"metrics", fmt.Sprintf("%t", cfg.MetricsEnabled)},
		}},
		{"token guard", [][2]string{
			{"token_guard", fmt.Sprintf("%t", cfg.TokenGuard)},
			{"token_warn_alert", fmt.Sprintf("%t", cfg.TokenWarnAlert)},
			{"token_auto_compact", fmt.Sprintf("%t", cfg.TokenAutoCompact)},
			{"token_warn", fmt.Sprintf("%d", cfg.TokenWarn)},
			{"token_critical", fmt.Sprintf("%d", cfg.TokenCritical)},
		}},
		{"auto-restart", [][2]string{
			{"auto_restart_max", fmt.Sprintf("%d", cfg.AutoRestartMax)},
			{"auto_restart_reset", cfg.AutoRestartReset},
		}},
		{"rate limit", [][2]string{
			{"rate_limit_auto_resume", fmt.Sprintf("%t", cfg.RateLimitAutoResume)},
			{"rate_limit_retry_interval", cfg.RateLimitRetryInterval},
			{"rate_limit_buffer", cfg.RateLimitBuffer},
		}},
		{"collaboration", [][2]string{
			{"collab_enabled", fmt.Sprintf("%t", cfg.CollabEnabled)},
			{"collab_interval", cfg.CollabInterval},
			{"collab_hint", fmt.Sprintf("%t", cfg.CollabHint)},
			{"branch_track_enabled", fmt.Sprintf("%t", cfg.BranchTrackEnabled)},
			{"branch_track_interval", cfg.BranchTrackInterval},
		}},
	}
	for _, g := range groups {
		fmt.Fprintf(out, "[%s]\n", g.title)
		tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
		for _, r := range g.rows {
			fmt.Fprintf(tw, "  %s\t%s\n", r[0], r[1])
		}
		tw.Flush()
		fmt.Fprintln(out)
	}
}

// webhookURLDisplay renders the webhook URL as set/unset rather than its value,
// since a Slack incoming-webhook URL embeds a secret token.
func webhookURLDisplay(url string) string {
	if url == "" {
		return "(unset)"
	}
	return "(set)"
}
