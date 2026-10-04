package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"github.com/srjn45/warden/internal/fastbrain"
)

// bugReportGH and bugReportDir are seams so tests never call a real gh or touch
// the user's home.
var (
	bugReportGH  = fastbrain.DefaultGH
	bugReportDir = fastbrain.DefaultCrashDir
)

type bugReportResult struct {
	ID        string `json:"id"`
	Submitted bool   `json:"submitted"`
	ViaGH     bool   `json:"via_gh,omitempty"`
	URL       string `json:"url,omitempty"`
}

func newBugReportCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "bug-report [id]",
		Short: "Review a staged crash draft and, only if you approve, file it as a GitHub issue",
		Long: `Show the sanitized crash draft warden staged under ~/.warden/crashes/<id>.json
(title, environment, sanitized stack) and ask whether to submit it to
https://github.com/srjn45/warden/issues. The default answer is N: nothing is
ever uploaded unless you type y.

On approval, an authenticated gh CLI creates the issue and its URL is printed;
otherwise a pre-filled new-issue link is printed for you to open. Declining
leaves the draft staged.

With no id, lists the staged drafts, newest first. Agents must never answer y
on the operator's behalf — tell the user to run this command instead.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := ""
			if len(args) == 1 {
				id = args[0]
			}
			return runBugReport(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout(), id, asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the outcome as JSON (never submits; prints the preview only)")
	return cmd
}

func runBugReport(ctx context.Context, in io.Reader, out io.Writer, id string, asJSON bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	dir, err := bugReportDir()
	if err != nil {
		return err
	}
	if id == "" {
		drafts, err := fastbrain.ListCrashDrafts(dir)
		if err != nil {
			return err
		}
		if len(drafts) == 0 {
			fmt.Fprintln(out, "no staged crash drafts")
			return nil
		}
		for _, d := range drafts {
			fmt.Fprintf(out, "%s  %s  %s\n", d.ID, d.CreatedAt.Format("2006-01-02 15:04"), d.Title)
		}
		return nil
	}
	d, err := fastbrain.LoadCrashDraft(dir, id)
	if errors.Is(err, fastbrain.ErrCrashNotFound) {
		return fmt.Errorf("no staged crash draft %q (see `warden bug-report`)", id)
	}
	if err != nil {
		return err
	}
	if asJSON { // machine mode never prompts and never submits
		return emitJSON(cmdOut(out), bugReportResult{ID: d.ID})
	}
	fmt.Fprintf(out, "Title:\n  %s\n\nEnvironment:\n  %s\n\nSanitized stack:\n%s\n\n", d.Title, d.Environment, d.Stack)
	fmt.Fprintf(out, "Submit this bug report to %s? [y/N] ", fastbrain.BugReportIssuesURL)
	line, _ := bufio.NewReader(in).ReadString('\n')
	if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
		fmt.Fprintln(out, "\nNothing was submitted; the draft stays staged.")
		return nil
	}
	link, viaGH, err := fastbrain.SubmitBugReport(ctx, bugReportGH(), d)
	if err != nil {
		return err
	}
	if viaGH {
		fmt.Fprintf(out, "Issue created: %s\n", link)
	} else {
		fmt.Fprintf(out, "gh is not authenticated — open this pre-filled issue link to submit:\n%s\n", link)
	}
	return nil
}
