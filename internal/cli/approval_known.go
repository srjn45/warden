package cli

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/srjn45/warden/internal/knownprompts"
)

// newApprovalKnownCmd builds `approval known`: inspect and prune the store of
// prompt shapes the Fast-Brain has learned.
func newApprovalKnownCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "known",
		Short: "Inspect and forget learned prompt shapes",
		Long: `Inspect and forget the known-prompts store.

When the Fast-Brain reads a permission prompt no backend parser recognizes, warden
remembers its SHAPE (templated question + option labels, never the concrete
command or path) so the next occurrence is recognized without a model call. Use
list to see what was learned and forget to drop an entry that was learned wrong.`,
	}
	SetCommandHelpMetadata(cmd, "coordinate", 40, "warden approval known", "", NodeNamespace)
	children := []*cobra.Command{newApprovalKnownListCmd(), newApprovalKnownForgetCmd()}
	for i, child := range children {
		SetCommandHelpMetadata(child, "coordinate", (i+1)*10, "warden approval known "+child.Name(), "", nodeKind(child))
		cmd.AddCommand(child)
	}
	return cmd
}

func newApprovalKnownListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List learned prompt shapes (id, backend, question, options, hits, last seen)",
		Example: `  warden approval known list
  warden approval known list --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			entries, err := clientFor(cmd).KnownPrompts(cmd.Context())
			if err != nil {
				return err
			}
			if entries == nil {
				entries = []knownprompts.Entry{}
			}
			if jsonRequested(cmd) {
				return emitJSON(cmd, entries)
			}
			fmt.Fprint(cmd.OutOrStdout(), formatKnownPrompts(entries, time.Now()))
			return nil
		},
	}
	addJSONFlag(cmd, "emit the entries as JSON")
	return cmd
}

func newApprovalKnownForgetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "forget [ID]",
		Short: "Forget one learned prompt shape, or all with --all",
		Long: `Forget a learned prompt shape by id (see approval known list). The prompt is
re-learned the next time it appears. With --all, empties the whole store after a
confirmation prompt (skip it with --yes). Forget actions are audit-logged.`,
		Example: `  warden approval known forget 3f9a1c0d2b7e4a65c8d1e0f2
  warden approval known forget --all --yes`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			all, _ := cmd.Flags().GetBool("all")
			yes, _ := cmd.Flags().GetBool("yes")
			c := clientFor(cmd)
			out := cmd.OutOrStdout()
			switch {
			case all && len(args) > 0:
				return fmt.Errorf("pass either an ID or --all, not both")
			case all:
				if !yes {
					if jsonRequested(cmd) {
						return fmt.Errorf("--json cannot prompt for confirmation to forget all; re-run with --yes")
					}
					if !confirmForgetAll(cmd.InOrStdin(), out) {
						fmt.Fprintln(out, "Nothing forgotten.")
						return nil
					}
				}
				n, err := c.ForgetAllKnownPrompts(cmd.Context())
				if err != nil {
					return err
				}
				if jsonRequested(cmd) {
					return emitJSON(cmd, map[string]int{"removed": n})
				}
				fmt.Fprintf(out, "forgot %d known prompt(s)\n", n)
			case len(args) == 1:
				if err := c.ForgetKnownPrompt(cmd.Context(), args[0]); err != nil {
					return err
				}
				if jsonRequested(cmd) {
					return emitJSON(cmd, map[string]string{"forgotten": args[0]})
				}
				fmt.Fprintf(out, "forgot %s\n", args[0])
			default:
				return fmt.Errorf("specify a prompt ID (see: warden approval known list) or --all")
			}
			return nil
		},
	}
	cmd.Flags().Bool("all", false, "forget every learned prompt shape")
	cmd.Flags().Bool("yes", false, "skip the confirmation prompt for --all")
	addJSONFlag(cmd, "emit the result as JSON")
	return cmd
}

func confirmForgetAll(in io.Reader, out io.Writer) bool {
	return confirmYN(in, out, "Forget ALL learned prompt shapes? [y/N] ")
}

// formatKnownPrompts renders the table. Options are joined with " | " and the
// affirmative one is marked with a trailing "*".
func formatKnownPrompts(entries []knownprompts.Entry, now time.Time) string {
	if len(entries) == 0 {
		return "(no known prompts)\n"
	}
	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tBACKEND\tQUESTION\tOPTIONS\tHITS\tLAST SEEN")
	for _, e := range entries {
		opts := make([]string, len(e.Options))
		for i, o := range e.Options {
			opts[i] = fmt.Sprintf("%d. %s", i+1, o)
			if i+1 == e.Affirmative {
				opts[i] += " *"
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\n", e.ID, e.Backend, e.Question,
			strings.Join(opts, " | "), e.Hits, ago(now, e.LastSeenAt))
	}
	_ = tw.Flush()
	b.WriteString("(* = option warden answers)\n")
	return b.String()
}

func ago(now, t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := now.Sub(t).Round(time.Minute)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}
