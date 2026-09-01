package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/srjn45/warden/internal/backendusage"
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
// the provider quota snapshot; spend, savings, and insights are grouped beneath it.
func newUsageNamespaceCmd() *cobra.Command {
	cmd := newUsageProviderCmd()
	SetCommandHelpMetadata(cmd, "observe", 50, "warden usage", "", NodeNamespace)
	children := []*cobra.Command{
		canonicalUsageCommand(newSpendCmd(), "spend"),
		canonicalUsageCommand(newSavingsCmd(), "savings"),
		canonicalUsageCommand(newInsightsCmd(), "insights"),
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
	fmt.Fprintln(w, "BACKEND\tPLAN\tSCOPE\tLIMIT\tMODELS\tUSED\tREMAINING\tRESETS\tSTATUS")
	for _, b := range s.Backends {
		plan := "-"
		if b.Account != nil && b.Account.Plan != "" {
			plan = b.Account.Plan
		}
		status := string(b.Status)
		if b.Stale {
			status += " (stale)"
		} else if b.Cached {
			status += " (cached)"
		}
		if len(b.Usage) == 0 {
			fmt.Fprintf(w, "%s\t%s\t-\t-\t-\t-\t-\t-\t%s\n", b.ID, plan, status)
			continue
		}
		for _, limit := range b.Usage {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", b.ID, plan, limit.Scope, limitLabel(limit), usageModelCell(limit), percentCell(limit.UsedPercent), percentCell(limit.RemainingPercent), resetCell(limit.ResetsAt), status)
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
	return strconv.FormatFloat(*v, 'f', -1, 64) + "%"
}
func resetCell(v *time.Time) string {
	if v == nil {
		return "-"
	}
	return v.In(time.Local).Format("2006-01-02 15:04 MST")
}
func limitLabel(limit backendusage.Limit) string {
	if limit.Label != "" {
		return limit.Label
	}
	if limit.DurationMinutes != nil {
		return durationCell(*limit.DurationMinutes)
	}
	return limit.ID
}

func usageModelCell(limit backendusage.Limit) string {
	selectors := append([]string(nil), limit.ModelFamilies...)
	selectors = append(selectors, limit.Models...)
	if len(selectors) == 0 {
		return "-"
	}
	return strings.Join(selectors, ",")
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
