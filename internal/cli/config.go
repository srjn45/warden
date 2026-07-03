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
			"with the config file path at the top. Edit the file by hand to change settings.\n\n" +
			"Settings are organized into namespaced blocks in the config file:\n" +
			"  rails.*    — guard/hook settings (git_redirect, root_guard, isolation_guard, …)\n" +
			"  tokens.*   — token-guard, budget-gate, and savings settings\n" +
			"  notify.*   — desktop notification and webhook settings\n" +
			"  worktree.* — worktree-retention and spawn-gate settings\n" +
			"  local_llm.*— local-model, REPL, and LLM-offload settings\n\n" +
			"Deprecated flat keys (e.g. token_guard, notify, local_llm_url) still load\n" +
			"and are automatically migrated to the namespaced form on `warden config init`.",
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
		Short: "Create the config file (or migrate it, adding any missing keys and upgrading deprecated flat keys to namespaced blocks)",
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
			{"pipeline.hint", fmt.Sprintf("%t", cfg.Pipeline.Hint)},
			{"pipeline.keep_done", fmt.Sprintf("%t", cfg.Pipeline.KeepDone)},
			{"tutorial", fmt.Sprintf("%t", cfg.Tutorial)},
		}},
		{"notifications & metrics (notify.*)", [][2]string{
			{"notify.enabled", fmt.Sprintf("%t", cfg.Notify.Enabled)},
			{"notify.webhook_enabled", fmt.Sprintf("%t", cfg.Notify.WebhookEnabled)},
			// The URL carries a secret (e.g. a Slack token), so show set/unset
			// rather than the value to keep it out of terminals and screenshots.
			{"notify.webhook_url", webhookURLDisplay(cfg.Notify.WebhookURL)},
			{"metrics", fmt.Sprintf("%t", cfg.MetricsEnabled)},
		}},
		{"token & budget guard (tokens.*)", [][2]string{
			{"tokens.guard", fmt.Sprintf("%t", cfg.Tokens.Guard)},
			{"tokens.warn_alert", fmt.Sprintf("%t", cfg.Tokens.WarnAlert)},
			{"tokens.auto_compact", fmt.Sprintf("%t", cfg.Tokens.AutoCompact)},
			{"tokens.warn", fmt.Sprintf("%d", cfg.Tokens.Warn)},
			{"tokens.critical", fmt.Sprintf("%d", cfg.Tokens.Critical)},
			{"tokens.budget_gate", fmt.Sprintf("%t", cfg.Tokens.BudgetGate)},
			{"tokens.budget_daily_usd", fmt.Sprintf("%.2f", cfg.Tokens.BudgetDailyUSD)},
			{"tokens.budget_weekly_usd", fmt.Sprintf("%.2f", cfg.Tokens.BudgetWeeklyUSD)},
			{"tokens.savings", fmt.Sprintf("%t", cfg.Tokens.Savings)},
			{"tokens.savings_samples", fmt.Sprintf("%t", cfg.Tokens.SavingsSamples)},
		}},
		{"worktree & spawn gate (worktree.*)", [][2]string{
			{"worktree.spawn_gate", fmt.Sprintf("%t", cfg.Worktree.SpawnGate)},
			{"worktree.spawn_gate_max_agents", fmt.Sprintf("%d", cfg.Worktree.SpawnGateMax)},
			{"worktree.keep_done", fmt.Sprintf("%t", cfg.Worktree.KeepDone)},
			{"worktree.auto_prune", fmt.Sprintf("%t", cfg.Worktree.AutoPrune)},
		}},
		{"rails / guards (rails.*)", [][2]string{
			{"rails.git_conventions", fmt.Sprintf("%t", cfg.Rails.GitConventions)},
			{"rails.git_redirect", fmt.Sprintf("%t", cfg.Rails.GitRedirect)},
			{"rails.check_redirect", fmt.Sprintf("%t", cfg.Rails.CheckRedirect)},
			{"rails.root_guard", fmt.Sprintf("%t", cfg.Rails.RootGuard)},
			{"rails.isolation_guard", fmt.Sprintf("%t", cfg.Rails.IsolationGuard)},
		}},
		{"auto-restart (auto_restart.*)", [][2]string{
			{"auto_restart.max", fmt.Sprintf("%d", cfg.AutoRestart.Max)},
			{"auto_restart.reset", cfg.AutoRestart.Reset},
		}},
		{"rate limit (rate_limit.*)", [][2]string{
			{"rate_limit.auto_resume", fmt.Sprintf("%t", cfg.RateLimit.AutoResume)},
			{"rate_limit.retry_interval", cfg.RateLimit.RetryInterval},
			{"rate_limit.buffer", cfg.RateLimit.Buffer},
		}},
		{"collaboration (collab.*, branch_track.*)", [][2]string{
			{"collab.enabled", fmt.Sprintf("%t", cfg.Collab.Enabled)},
			{"collab.interval", cfg.Collab.Interval},
			{"collab.hint", fmt.Sprintf("%t", cfg.Collab.Hint)},
			{"branch_track.enabled", fmt.Sprintf("%t", cfg.BranchTrack.Enabled)},
			{"branch_track.interval", cfg.BranchTrack.Interval},
		}},
		{"local model / REPL (local_llm.*)", [][2]string{
			{"local_llm.enabled", fmt.Sprintf("%t", cfg.LocalLLM.Enabled)},
			{"local_llm.url", cfg.LocalLLM.URL},
			{"local_llm.model", cfg.LocalLLM.Model},
			{"local_llm.timeout", cfg.LocalLLM.Timeout},
			{"local_llm.escalate", fmt.Sprintf("%t", cfg.LocalLLM.Escalate)},
			{"local_llm.tier", cfg.LocalLLM.Tier},
			{"local_llm.classifier", cfg.LocalLLM.Classifier},
			{"local_llm.repl", fmt.Sprintf("%t", cfg.LocalLLM.Repl)},
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
