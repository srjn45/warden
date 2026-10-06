package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"github.com/srjn45/warden/internal/client"
)

func init() {
	const (
		start  = "this command was removed: start a run with `wd plan run <plan-id> --mode autopilot`."
		pause  = "this command was removed: use `wd plan pause`."
		stop   = "this command was removed: use `wd plan stop`."
		status = "this command was removed: use `wd autopilot status`."
	)
	removedCommandHints["warden autopilot"] = map[string]string{
		"init":       "`wd autopilot init` was removed: create the plan with `wd plan create`, then start it with `wd plan run <plan-id> --mode autopilot`.",
		"enable":     start,
		"on":         start,
		"register":   start,
		"start":      start,
		"disable":    pause,
		"off":        pause,
		"pause":      pause,
		"resume":     "this command was removed: use `wd plan resume`.",
		"stop":       stop,
		"unregister": stop,
		"list":       status,
		"run":        status,
	}
}

func newAutopilotCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "autopilot",
		Short: "Show autopilot status and land worker branches",
		Long: "Autopilot is the unattended Plan execution mode. There is no per-repo switch:\n" +
			"start a run explicitly with `warden plan run <plan-id> --mode autopilot` and\n" +
			"control it with `warden plan pause|resume|stop`. This namespace shows status\n" +
			"(`status`) and lands worker branches (`land`).\n" +
			"The daemon merges worker PRs into the integration branch itself once their\n" +
			"gate is green; `land` is the manual fallback. A run ends with one final PR to\n" +
			"the default branch that you merge.\n" +
			"Configure the feature under the `autopilot` block in the config file.",
	}
	SetCommandHelpMetadata(cmd, "run", 30, "warden autopilot", "", NodeNamespace)

	children := []*cobra.Command{
		newAutopilotStatusCmd(),
		newAutopilotLandCmd(),
	}
	for i, child := range children {
		SetCommandHelpMetadata(child, "run", (i+1)*10, "warden autopilot "+child.Name(), "", nodeKind(child))
		cmd.AddCommand(child)
	}
	return cmd
}

func newAutopilotStatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "List every autopilot run",
		Long: "The all-runs view: one table row per autopilot run (run id, name, state, plan,\n" +
			"gate, branch and task progress; the repo column appears only when runs span\n" +
			"more than one repo). Runs that are waiting, degraded or need attention print\n" +
			"indented detail lines under their row (next step, resting until, the guardian's\n" +
			"diagnosis, the final PR). Use `wd plan show <id> --watch` for the view of one\n" +
			"plan. --json emits the raw result.",
		Args: cobra.NoArgs,
		RunE: runAutopilotStatus,
	}
	addJSONFlag(cmd, "emit the raw autopilot status as JSON")
	return cmd
}

func runAutopilotStatus(cmd *cobra.Command, _ []string) error {
	st, err := clientFor(cmd).GetAutopilot(cmd.Context())
	if err != nil {
		return err
	}
	if jsonRequested(cmd) {
		return printJSON(cmd.OutOrStdout(), st)
	}
	if len(st.Runs) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "no autopilot runs")
		fmt.Fprintln(cmd.OutOrStdout(), "start one with: wd plan run <plan-id> --mode autopilot")
		return nil
	}
	return printAutopilotRuns(cmd.OutOrStdout(), st)
}

// autopilotProgress is the task rollup "landed/total" ("-" with no tasks).
func autopilotProgress(r client.AutopilotRunStatus) string {
	total := r.Tasks.Pending + r.Tasks.InProgress + r.Tasks.Landed + r.Tasks.Failed
	if total == 0 {
		total = len(r.PlanTasks)
	}
	if total == 0 {
		return "-"
	}
	return fmt.Sprintf("%d/%d", r.Tasks.Landed, total)
}

// printAutopilotRuns renders an aligned table of runs, with indented detail
// lines under any run that needs them. The table is aligned as one block and
// the detail lines are spliced in afterwards so they do not break alignment.
func printAutopilotRuns(w io.Writer, st client.AutopilotStatus) error {
	repos := map[string]bool{}
	for _, r := range st.Runs {
		repos[r.Repo] = true
	}
	showRepo := len(repos) > 1
	var buf strings.Builder
	tw := tabwriter.NewWriter(&buf, 0, 2, 2, ' ', 0)
	head := "RUN\tNAME\tSTATE\tPLAN\tGATE\tBRANCH\tPROGRESS"
	if showRepo {
		head += "\tREPO"
	}
	fmt.Fprintln(tw, head)
	for _, r := range st.Runs {
		row := fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s\t%s", r.RunID, dash(r.Name), r.State,
			dash(r.PlanID), dash(r.Gate), dash(r.IntegrationBranch), autopilotProgress(r))
		if showRepo {
			row += "\t" + dash(r.Repo)
		}
		fmt.Fprintln(tw, row)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	fmt.Fprintln(w, lines[0])
	for i, r := range st.Runs {
		fmt.Fprintln(w, lines[i+1])
		for _, l := range autopilotRunDetails(r) {
			fmt.Fprintln(w, "    "+l)
		}
	}
	return nil
}

// autopilotRunDetails are the per-run detail lines (no leading indent).
func autopilotRunDetails(r client.AutopilotRunStatus) []string {
	var out []string
	if r.Backoff != nil {
		out = append(out, "backoff: "+backoffSummary(r.Backoff))
	}
	if r.LastProgressAt != "" {
		out = append(out, fmt.Sprintf("last progress: %s (watchdog: %s)", r.LastProgressAt, dash(r.Watchdog)))
	}
	if r.NextStep != nil {
		line := "next: " + r.NextStep.Action
		if r.NextStep.At != "" {
			line += " at " + r.NextStep.At
		}
		if r.NextStep.Owner != "" {
			line += " (" + r.NextStep.Owner + ")"
		}
		out = append(out, line)
	}
	if r.RestingUntil != "" {
		out = append(out, "resting until: "+r.RestingUntil)
	}
	out = append(out, r.SurfaceLines()...)
	if r.NeedsAttention != "" {
		out = append(out, "NEEDS ATTENTION: "+r.NeedsAttention)
	}
	return out
}

func backoffSummary(b *client.AutopilotBackoff) string {
	if b == nil {
		return "-"
	}
	out := fmt.Sprintf("stage %d", b.Stage)
	if b.Kind != "" {
		out += " kind " + b.Kind
	}
	if b.NextRetryAt != "" {
		out += " retry " + b.NextRetryAt
	}
	if b.LastError != "" {
		out += " (" + b.LastError + ")"
	}
	return out
}

// landAdvice maps each land failure kind to a plain sentence and a next step.
var landAdvice = map[string]string{
	"not_found":     "no agent or branch matches that name. Next: check `wd autopilot status` or `wd ls` for the exact name.",
	"not_owned":     "that branch exists but is not owned by an autopilot run, so autopilot will not merge it. Next: merge it yourself, or land a worker branch from `wd autopilot status`.",
	"run_disabled":  "the owning autopilot run is paused or stopped. Next: resume it with `wd plan resume`, then land again.",
	"wrong_base":    "the pull request does not target the run's integration branch. Next: retarget the PR to the integration branch and land again.",
	"gate_pending":  "the merge gate has not finished yet. Next: wait for it, or check the PR's checks, then land again.",
	"gate_red":      "the merge gate failed. Next: fix the failing checks on the worker branch, push, then land again.",
	"ci_missing":    "the gate requires CI but the PR has no CI results. Next: make sure CI runs on the PR, or change the run's gate mode.",
	"not_mergeable": "the pull request has conflicts with the integration branch. Next: sync the branch (`wd sync --base <integration-branch>`), push, then land again.",
}

// landFailure turns a land precondition failure into the single error the CLI
// prints: a plain sentence, a next step, and the daemon's detail when present.
func landFailure(le *client.AutopilotLandError) error {
	msg, ok := landAdvice[string(le.Kind)]
	if !ok {
		msg = "the daemon refused the merge."
	}
	out := fmt.Sprintf("not landed (%s): %s", le.Kind, msg)
	if le.Detail != "" {
		out += " Detail: " + le.Detail
	}
	return errors.New(out)
}

// newAutopilotLandCmd is `warden autopilot land`: the manual, gated merge of one
// autopilot worker branch into the integration branch. It mirrors the MCP `land`
// tool the brain uses.
func newAutopilotLandCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "land <agent-or-branch>",
		Short: "Land an autopilot worker branch into the integration branch",
		Long: "Merges one autopilot worker branch into the integration branch. The daemon\n" +
			"does this itself when a worker PR's gate is green; `land` is the manual\n" +
			"fallback for an operator or manager, with the same preconditions.\n\n" +
			"It checks that the owning run is active, the branch is autopilot-owned, a PR\n" +
			"targets the integration branch, the gate is green for the PR head, and the PR\n" +
			"is mergeable. It then merges with the configured strategy, deletes the worker\n" +
			"branch if configured, and records the landing. Re-running after a merge\n" +
			"reports already-landed without merging again.\n\n" +
			"If a check fails, nothing is merged and one error names the reason:\n\n" +
			"  not_found      no agent or branch matches the name\n" +
			"  not_owned      the branch exists but no autopilot run owns it\n" +
			"  run_disabled   the run is paused or stopped\n" +
			"  wrong_base     the PR does not target the integration branch\n" +
			"  gate_pending   the gate has not finished yet\n" +
			"  gate_red       the gate failed\n" +
			"  ci_missing     the gate needs CI but the PR has none\n" +
			"  not_mergeable  the PR has conflicts",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := clientFor(cmd).Land(cmd.Context(), args[0])
			if err != nil {
				var le *client.AutopilotLandError
				if errors.As(err, &le) {
					if jsonRequested(cmd) {
						if perr := printJSON(cmd.OutOrStdout(), map[string]any{"landed": false, "kind": le.Kind, "detail": le.Detail}); perr != nil {
							return perr
						}
					}
					return landFailure(le)
				}
				return err
			}
			if jsonRequested(cmd) {
				return printJSON(cmd.OutOrStdout(), res)
			}
			if res.AlreadyLanded {
				fmt.Fprintf(cmd.OutOrStdout(), "already landed: %s @ %s (PR #%d)\n", res.Branch, res.SHA, res.PR)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "landed %s @ %s (PR #%d)\n", res.Branch, res.SHA, res.PR)
			return nil
		},
	}
	addJSONFlag(cmd, "emit the raw land result as JSON")
	return cmd
}
