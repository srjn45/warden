package cli

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/srjn45/warden/internal/client"
)

func newBranchesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "branches",
		Short: "Per-agent CI + branch-vs-main status",
		Long: "Show each tracked agent's branch: its latest GitHub CI run and how it " +
			"sits against origin/main (behind/ahead/merged).\n\n" +
			"Read-only. Requires the branch tracker to be enabled (branch_track.enabled); " +
			"a disabled tracker reports no branches.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonOut, _ := cmd.Flags().GetBool("json")
			statuses, err := clientFor(cmd).BranchStatuses(cmd.Context())
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if jsonOut {
				return printJSON(out, statuses)
			}
			if len(statuses) == 0 {
				fmt.Fprintln(out, "No tracked branches.")
				return nil
			}
			fmt.Fprintf(out, "Branch status (%d):\n\n", len(statuses))
			for _, s := range statuses {
				fmt.Fprintf(out, "%s  [%s]\n", s.Branch, branchAgentLabel(s))
				fmt.Fprintf(out, "  CI:    %s\n", formatCI(s.CI))
				fmt.Fprintf(out, "  main:  %s\n", formatBranchVsMain(s))
				fmt.Fprintln(out)
			}
			return nil
		},
	}
	cmd.Flags().Bool("json", false, "output the branch statuses as JSON")
	return cmd
}

func branchAgentLabel(s client.BranchStatus) string {
	if s.Name != "" {
		return fmt.Sprintf("%s (%s)", s.AgentID, s.Name)
	}
	return s.AgentID
}

func formatCI(ci client.CIStatus) string {
	switch ci.State {
	case "success":
		s := "✅ success"
		if ci.Workflow != "" {
			s += " — " + ci.Workflow
		}
		return s
	case "failure":
		s := "❌ failure"
		if ci.Workflow != "" {
			s += " — " + ci.Workflow
		}
		if ci.URL != "" {
			s += " (" + ci.URL + ")"
		}
		return s
	case "pending":
		s := "⏳ pending"
		if ci.Workflow != "" {
			s += " — " + ci.Workflow
		}
		return s
	default:
		return "— no runs"
	}
}

func formatBranchVsMain(s client.BranchStatus) string {
	if s.Merged {
		return "✅ merged into main"
	}
	switch {
	case s.Behind == 0 && s.Ahead == 0:
		return "even with main"
	default:
		return fmt.Sprintf("%d behind, %d ahead", s.Behind, s.Ahead)
	}
}
