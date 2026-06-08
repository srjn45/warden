package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/srajanpathak/warden/internal/digest"
)

func newDigestCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "digest <TICKET>",
		Short: "Summarize what an agent accomplished (files, branch, turns, narrative)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := clientFor(cmd).Digest(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
				return printJSON(cmd.OutOrStdout(), d)
			}
			fmt.Fprint(cmd.OutOrStdout(), formatDigest(d))
			return nil
		},
	}
	cmd.Flags().Bool("json", false, "output as JSON")
	return cmd
}

// formatDigest renders the human layout: summary paragraph, file table with
// +/- columns, then branch / turns / status.
func formatDigest(d *digest.Digest) string {
	var b strings.Builder
	if d.Summary != "" {
		b.WriteString(d.Summary)
		b.WriteString("\n\n")
	}
	if len(d.Files) == 0 {
		b.WriteString("files: (no files touched)\n")
	} else {
		b.WriteString("files:\n")
		for _, f := range d.Files {
			mark := " "
			if f.Edited {
				mark = "*"
			}
			fmt.Fprintf(&b, "  %s %-40s +%-4d -%-4d\n", mark, f.Path, f.Added, f.Removed)
		}
	}
	branch := d.Branch
	if branch == "" {
		branch = "—"
	}
	fmt.Fprintf(&b, "\nbranch: %s   turns: %d   status: %s\n", branch, d.Turns, d.Status)
	return b.String()
}
