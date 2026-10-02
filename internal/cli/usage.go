package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/srjn45/warden/internal/backendusage"
	"github.com/srjn45/warden/internal/client"
)

type partialResultError struct{}

func (partialResultError) Error() string { return "partial usage results" }

// ExitCode maps typed command outcomes without coupling Cobra to os.Exit.
func ExitCode(err error) int {
	var partial partialResultError
	if errors.As(err, &partial) {
		return 2
	}
	return 1
}

// newUsageNamespaceCmd is the canonical usage namespace. Its bare action remains
// the provider quota snapshot; spend, savings, insights, and recover are grouped beneath it.
func newUsageNamespaceCmd() *cobra.Command {
	cmd := newUsageProviderCmd()
	SetCommandHelpMetadata(cmd, "observe", 50, "warden usage", "", NodeNamespace)
	children := []*cobra.Command{
		canonicalUsageCommand(newSpendCmd(), "spend"),
		canonicalUsageCommand(newSavingsCmd(), "savings"),
		canonicalUsageCommand(newInsightsCmd(), "insights"),
		newUsageRecoverCmd(),
	}
	for i, child := range children {
		SetCommandHelpMetadata(child, "observe", (i+1)*10, "warden usage "+child.Name(), "", nodeKind(child))
		cmd.AddCommand(child)
	}
	return cmd
}

func newUsageProviderCmd() *cobra.Command {
	var jsonOutput, refresh bool
	cmd := &cobra.Command{
		Use: "usage", Short: "Show provider usage for subscription backends", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			snapshot, err := clientFor(cmd).Usage(cmd.Context(), refresh)
			if err != nil {
				return err
			}
			if jsonOutput {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetEscapeHTML(false)
				if err := enc.Encode(snapshot); err != nil {
					return err
				}
			} else if err := printUsage(cmd, snapshot); err != nil {
				return err
			}
			for _, b := range snapshot.Backends {
				if usageIsPartial(b) {
					return partialResultError{}
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "print the stable JSON document")
	cmd.Flags().BoolVar(&refresh, "refresh", false, "bypass the daemon's fresh usage cache")
	return cmd
}

func usageIsPartial(b backendusage.BackendResult) bool {
	return b.Stale || operationalFailure(b.Status)
}

func operationalFailure(s backendusage.Status) bool {
	return s != backendusage.StatusOK && s != backendusage.StatusUnsupported
}

func printUsage(cmd *cobra.Command, s backendusage.Snapshot) error {
	out := cmd.OutOrStdout()
	if len(s.Backends) == 0 {
		_, err := fmt.Fprintln(out, "No subscription-tier backends configured.")
		return err
	}
	w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "AICLI\tBUCKET\tUSED\tREMAINING\tRESETS")
	for _, b := range s.Backends {
		if len(b.Usage) == 0 {
			fmt.Fprintf(w, "%s\t-\t-\t-\t-\n", b.ID)
			continue
		}
		for _, limit := range b.Usage {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", b.ID, bucketLabel(limit), percentCell(limit.UsedPercent), percentCell(limit.RemainingPercent), resetCell(limit.ResetsAt))
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	for _, b := range s.Backends {
		if b.Error != nil {
			fmt.Fprintf(out, "%s: %s\n", b.ID, b.Error.Message)
		}
	}
	return nil
}

func percentCell(v *float64) string {
	if v == nil {
		return "-"
	}
	rounded := math.Round(*v*100) / 100
	return strconv.FormatFloat(rounded, 'f', -1, 64) + "%"
}

func resetCell(v *time.Time) string {
	if v == nil {
		return "-"
	}
	return v.In(time.Local).Format("2006-01-02 15:04 MST")
}

func bucketLabel(limit backendusage.Limit) string {
	switch strings.ToLower(strings.TrimSpace(limit.Label)) {
	case "gemini models", "gemini":
		return "Gemini"
	case "non-gemini models", "non-gemini":
		return "Non-Gemini"
	case "session (5-hour)", "session":
		return "Session"
	case "codex primary", "primary":
		return "Primary"
	}
	if limit.Label != "" {
		return limit.Label
	}
	if limit.Scope != "" {
		return limit.Scope
	}
	if limit.DurationMinutes != nil {
		return durationCell(*limit.DurationMinutes)
	}
	return limit.ID
}

func durationCell(minutes int) string {
	if minutes%(7*24*60) == 0 {
		return fmt.Sprintf("%dw", minutes/(7*24*60))
	}
	if minutes%(24*60) == 0 {
		return fmt.Sprintf("%dd", minutes/(24*60))
	}
	if minutes%60 == 0 {
		return fmt.Sprintf("%dh", minutes/60)
	}
	return fmt.Sprintf("%dm", minutes)
}

func canonicalUsageCommand(cmd *cobra.Command, name string) *cobra.Command {
	parts := strings.SplitN(cmd.Use, " ", 2)
	legacyName := parts[0]
	rewriteUsageHelpPaths(cmd, legacyName, name)
	cmd.Use = name
	if len(parts) == 2 {
		cmd.Use += " " + parts[1]
	}
	cmd.Aliases = nil
	return cmd
}

func rewriteUsageHelpPaths(cmd *cobra.Command, legacyName, canonicalName string) {
	replacer := strings.NewReplacer(
		"warden "+legacyName, "warden usage "+canonicalName,
		"wd "+legacyName, "wd usage "+canonicalName,
		"warden cost "+canonicalName, "warden usage "+canonicalName,
		"wd cost "+canonicalName, "wd usage "+canonicalName,
	)
	cmd.Long = replacer.Replace(cmd.Long)
	cmd.Example = replacer.Replace(cmd.Example)
}

// newUsageRecoverCmd backs `wd usage recover`: operator-triggered one-shot
// reconciliation using the same flow as the background usage poller.
func newUsageRecoverCmd() *cobra.Command {
	var (
		dryRun, jsonOutput bool
		aiCli, project     string
		maxParallelSwaps   int
	)
	cmd := &cobra.Command{
		Use:   "recover",
		Short: "Fetch fresh usage and reconcile exhausted buckets to affected agents",
		Long: strings.TrimSpace(`
Operator-triggered one-shot usage reconciliation.

Fetches fresh supported provider usage snapshots (never treats cached/stale data
as forced exhaustion), calculates bucket-to-agent impact, and — unless
--dry-run — invokes the same backend recovery coordinator path as the
background usage reconciliation loop.

Default invocation is an explicit operator action and may start recovery for
affected agents. Use --dry-run to print snapshots, impact, and candidate
decisions without claiming fences or starting recovery.

Optional --ai-cli and --project filters limit which agents may be affected;
unrelated agents are left untouched. --max-parallel-swaps temporarily overrides
the coordinator's bounded concurrency for this invocation only.
`),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if maxParallelSwaps < 0 {
				return errors.New("--max-parallel-swaps must be >= 1 when set")
			}
			result, err := clientFor(cmd).UsageRecover(cmd.Context(), client.UsageRecoverParams{
				DryRun:           dryRun,
				AiCli:            aiCli,
				Project:          project,
				MaxParallelSwaps: maxParallelSwaps,
			})
			if err != nil {
				return err
			}
			if jsonOutput {
				return printJSON(cmd.OutOrStdout(), result)
			}
			return printUsageRecover(cmd, result)
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "fetch fresh snapshots and calculate impact without starting recovery")
	cmd.Flags().StringVar(&aiCli, "ai-cli", "", "limit reconciliation to agents on this AI CLI / provider")
	cmd.Flags().StringVar(&project, "project", "", "limit reconciliation to agents in this project path")
	cmd.Flags().IntVar(&maxParallelSwaps, "max-parallel-swaps", 0, "temporary bounded concurrency override for this invocation (0 = daemon default)")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "print the structured recover response as JSON")
	return cmd
}

func printUsageRecover(cmd *cobra.Command, result client.UsageRecoverResult) error {
	out := cmd.OutOrStdout()
	mode := "apply"
	if result.DryRun {
		mode = "dry-run"
	}
	fmt.Fprintf(out, "usage recover (%s)\n", mode)
	if result.MaxParallelSwaps > 0 {
		fmt.Fprintf(out, "max_parallel_swaps: %d\n", result.MaxParallelSwaps)
	}
	fmt.Fprintln(out)

	fmt.Fprintln(out, "SNAPSHOTS")
	if len(result.Snapshots) == 0 {
		fmt.Fprintln(out, "  (none)")
	} else {
		for _, snap := range result.Snapshots {
			fmt.Fprintf(out, "  %s fingerprint=%s route=%s freshness=%s authoritative=%v revision=%d status=%s\n",
				snap.Domain.Provider, snap.Domain.ProfileFingerprint, snap.Domain.Route,
				snap.Freshness, snap.Authoritative, snap.Revision, snap.SourceStatus)
			for _, b := range snap.Buckets {
				reset := "-"
				if b.ResetsAt != nil {
					reset = b.ResetsAt.In(time.Local).Format(time.RFC3339)
				}
				fmt.Fprintf(out, "    bucket %s state=%s resets=%s\n", b.Key, b.State, reset)
			}
		}
	}
	fmt.Fprintln(out)

	fmt.Fprintln(out, "IMPACT")
	fmt.Fprintf(out, "  exhausted_buckets: %d\n", len(result.Impact.ExhaustedBuckets))
	for _, b := range result.Impact.ExhaustedBuckets {
		fmt.Fprintf(out, "    %s/%s bucket=%s revision=%d source=%s\n",
			b.Provider, b.AccountFingerprint, b.BucketKey, b.SnapshotRevision, b.Source)
	}
	fmt.Fprintf(out, "  affected_agents: %d\n", len(result.Impact.AffectedAgents))
	for _, a := range result.Impact.AffectedAgents {
		fmt.Fprintf(out, "    %s bucket=%s\n", a.AgentID, a.BucketKey)
	}
	fmt.Fprintf(out, "  skipped_agents: %d\n", len(result.Impact.SkippedAgents))
	for _, a := range result.Impact.SkippedAgents {
		detail := a.Detail
		if detail != "" {
			detail = " (" + detail + ")"
		}
		fmt.Fprintf(out, "    %s reason=%s%s\n", a.AgentID, a.Reason, detail)
	}
	fmt.Fprintf(out, "  stale_or_unknown: %d\n", len(result.Impact.StaleOrUnknown))
	for _, s := range result.Impact.StaleOrUnknown {
		fmt.Fprintf(out, "    %s bucket=%s reason=%s freshness=%s\n", s.Provider, s.BucketKey, s.Reason, s.Freshness)
	}
	fmt.Fprintln(out)

	fmt.Fprintln(out, "OUTCOMES")
	if len(result.Outcomes) == 0 {
		fmt.Fprintln(out, "  (none)")
		return nil
	}
	for _, oc := range result.Outcomes {
		fmt.Fprintf(out, "  %s outcome=%s", oc.AgentID, oc.Outcome)
		if oc.Phase != "" {
			fmt.Fprintf(out, " phase=%s", oc.Phase)
		}
		if oc.BucketKey != "" {
			fmt.Fprintf(out, " bucket=%s", oc.BucketKey)
		}
		if oc.Selected.BackendID != "" {
			fmt.Fprintf(out, " selected=%s/%s", oc.Selected.BackendID, oc.Selected.ModelID)
		}
		fmt.Fprintln(out)
		if len(oc.Candidates) > 0 {
			parts := make([]string, 0, len(oc.Candidates))
			for _, c := range oc.Candidates {
				parts = append(parts, c.BackendID+"/"+c.ModelID)
			}
			fmt.Fprintf(out, "    candidates: %s\n", strings.Join(parts, ", "))
		}
		if oc.Reason != "" {
			fmt.Fprintf(out, "    reason: %s\n", oc.Reason)
		}
	}
	return nil
}
