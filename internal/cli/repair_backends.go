package cli

// `warden repair backends`: the supported offline verify/repair surface for the
// backend registry (#841; docs/specs/2026-10-09-backend-registry-integrity-contract.md
// §10). It is a thin CLI over backendstore.Verify / backendstore.Repair: all
// classification, backup and resolution rules live there.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
	"github.com/srjn45/scriva/engine"

	"github.com/srjn45/warden/internal/backendstore"
	"github.com/srjn45/warden/internal/config"
	"github.com/srjn45/warden/internal/ownerlock"
)

// Exit codes of `warden repair backends`. 2 is deliberately unused: it already
// means "partial results" elsewhere in the CLI (see ExitCode).
const (
	repairExitOK               = 0 // clean (no-op), or repaired and verified
	repairExitError            = 1 // unexpected failure (I/O, unreadable registry, bad flags)
	repairExitOwned            = 3 // refused: a running process owns the data dir; nothing read or changed
	repairExitRecoveryRequired = 4 // ambiguous findings, or a repair that failed and was rolled back
	repairExitUnconfirmed      = 5 // confirmation missing or declined; nothing changed
	repairExitRepairable       = 6 // --dry-run only: safely recoverable findings are present
)

// Statuses reported in the `status` field.
const (
	repairStatusAbsent           = "absent"
	repairStatusClean            = "clean"
	repairStatusRecoverable      = "recoverable"
	repairStatusRecoveryRequired = "recovery-required"
	repairStatusRepaired         = "repaired"
	repairStatusCancelled        = "cancelled"
	repairStatusOwned            = "owned"
)

// Classifications reported per finding and per collection.
const (
	classSafelyRecoverable = "safely-recoverable"
	classRecoveryRequired  = "recovery-required"
)

// exitCodeError carries a command-specific process exit code through cobra.
type exitCodeError struct {
	code int
	err  error
}

func (e *exitCodeError) Error() string { return e.err.Error() }
func (e *exitCodeError) Unwrap() error { return e.err }

func withExitCode(code int, format string, a ...any) error {
	return &exitCodeError{code: code, err: fmt.Errorf(format, a...)}
}

// registryPreservation names the user-owned facts each registry collection
// holds: what a finding in that collection could affect.
var registryPreservation = map[string][]string{
	"backends":          {"backend rows", "tiers", "enabled flags", "default backend", "settings"},
	"models":            {"models"},
	"role_tiers":        {"role tiers"},
	"handover_settings": {"handover settings"},
	"quotas":            {"quotas"},
	"rl_cooldowns":      {"rate-limit cooldowns"},
}

type repairOwner struct {
	PID         int    `json:"pid,omitempty"`
	Kind        string `json:"kind,omitempty"`
	Launch      string `json:"launch,omitempty"`
	Version     string `json:"version,omitempty"`
	Addr        string `json:"addr,omitempty"`
	StopCommand string `json:"stop_command"`
}

type repairFinding struct {
	Severity       string `json:"severity"`
	Code           string `json:"code"`
	Classification string `json:"classification"`
	Segment        string `json:"segment,omitempty"`
	Offset         int64  `json:"offset,omitempty"`
	ID             uint64 `json:"id,omitempty"`
	Message        string `json:"message"`
}

type repairCollection struct {
	Name              string          `json:"name"`
	Verdict           string          `json:"verdict"`
	Classification    string          `json:"classification"`
	Severities        []string        `json:"severities,omitempty"`
	Codes             []string        `json:"codes,omitempty"`
	Reasons           []string        `json:"reasons,omitempty"`
	StaleRevisions    int             `json:"stale_revisions"`
	PreservationRisks []string        `json:"preservation_risks"`
	Findings          []repairFinding `json:"findings"`
}

// repairBackendsOutput is the stable `--json` document (and the model the text
// output is rendered from).
type repairBackendsOutput struct {
	Command           string                           `json:"command"`
	Mode              string                           `json:"mode"` // dry-run | repair
	Dir               string                           `json:"dir"`
	Status            string                           `json:"status"`
	ExitCode          int                              `json:"exit_code"`
	Mutated           bool                             `json:"mutated"`
	Owner             *repairOwner                     `json:"owner,omitempty"`
	Collections       []repairCollection               `json:"collections"`
	PreservationRisks []string                         `json:"preservation_risks"`
	ResolutionRule    string                           `json:"resolution_rule,omitempty"`
	Discarded         []backendstore.DiscardedRevision `json:"discarded,omitempty"`
	ReportPath        string                           `json:"report_path"`
	BackupPath        string                           `json:"backup_path"`
	BackupVerified    bool                             `json:"backup_verified"`
	Error             string                           `json:"error,omitempty"`
	NextAction        string                           `json:"next_action"`
}

// stdinIsInteractive reports whether in is a terminal a human can answer on.
// It is a seam so tests can exercise the prompt without a TTY.
var stdinIsInteractive = func(in io.Reader) bool {
	f, ok := in.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func newRepairBackendsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "backends",
		Short: "Verify or repair the backend registry offline, backup-first",
		Long: `Offline verify/repair of the backend registry (<data>/backends): backend rows,
tiers, enabled flags, default backend, settings, models, role tiers, quotas,
rate-limit cooldowns and handover settings.

The daemon must be stopped: the command refuses while any process owns the data
directory and names the owner and the stop command (with the systemd user
service: systemctl --user stop warden). Never start a second "warden daemon"
beside the service to work around it.

  warden repair backends --dry-run   read-only: classify every finding, change nothing
  warden repair backends             repair after an explicit confirmation

Each finding is classified as safely-recoverable (provably stale revision
history or a stale derived index) or recovery-required (ambiguous history or
damaged bytes). Repair takes a verified backup first, removes only provably
stale revisions (each one is listed in the report and kept in the backup), and
verifies the result; on any failure the backup is restored. Ambiguous history is
never resolved automatically: it is reported and the registry is left untouched.
A clean registry is a no-op.

Repair needs confirmation: answer the prompt, or pass --yes when not on a
terminal. Without either it refuses and changes nothing.

Exit codes:
  0  clean (nothing to do), or repaired and verified
  1  unexpected error
  3  refused: a running process owns the data directory
  4  recovery required: ambiguous findings, or the repair failed and was rolled back
  5  confirmation missing or declined
  6  --dry-run found safely recoverable findings (run without --dry-run to repair)`,
		Example: `  systemctl --user stop warden
  warden repair backends --dry-run
  warden repair backends
  systemctl --user start warden

  warden repair backends --yes --json`,
		Args: cobra.NoArgs,
		RunE: runRepairBackends,
	}
	cmd.Flags().Bool("dry-run", false, "verify and report only; read-only, changes nothing")
	cmd.Flags().BoolP("yes", "y", false, "confirm the repair without prompting (required when not on a terminal)")
	cmd.Flags().String("backup-dir", "", "parent directory for the verified backup and the report (default <data>/backend-registry-backups; must be outside <data>/backends)")
	cmd.Flags().Bool("json", false, "print the machine-readable report")
	return cmd
}

func runRepairBackends(cmd *cobra.Command, _ []string) error {
	cfg := config.Load(configPathFor(cmd))
	dry, _ := cmd.Flags().GetBool("dry-run")
	yes, _ := cmd.Flags().GetBool("yes")
	jsonOut, _ := cmd.Flags().GetBool("json")
	backupDir, _ := cmd.Flags().GetString("backup-dir")
	dir := filepath.Join(cfg.DataDir, "backends")

	out := &repairBackendsOutput{Command: backendstore.RepairCommand, Mode: "repair", Dir: dir,
		Collections: []repairCollection{}, PreservationRisks: []string{}}
	if dry {
		out.Command, out.Mode = backendstore.RepairDryRunCommand, "dry-run"
	}
	audit := func(outcome string, err error) {
		slog.Warn("audit: backend-registry repair attempt", "audit", true, "action", "repair_backends",
			"mode", out.Mode, "outcome", outcome, "dir", dir, "uid", os.Geteuid(), "err", err)
	}
	finish := func(status string, code int, cause error) error {
		out.Status, out.ExitCode = status, code
		if cause != nil {
			out.Error = cause.Error()
		}
		if out.NextAction == "" {
			out.NextAction = repairNextAction(out, dry)
		}
		audit(status, cause)
		if perr := printRepairBackends(cmd.OutOrStdout(), out, jsonOut); perr != nil {
			return perr
		}
		if code == repairExitOK {
			return nil
		}
		return withExitCode(code, "backend registry %s: %s", status, out.NextAction)
	}
	owned := func(oe *ownerlock.OwnedError) error {
		out.Owner = ownerOf(oe)
		return finish(repairStatusOwned, repairExitOwned, oe)
	}

	// Never read or open the registry under a foreign owner: a live writer makes
	// a verification meaningless and a repair unsafe.
	oe, err := probeDataDirOwner(cfg.DataDir)
	if err != nil {
		return finish("error", repairExitError, fmt.Errorf("probe data directory owner: %w", err))
	}
	if oe != nil {
		return owned(oe)
	}
	if _, serr := os.Stat(dir); errors.Is(serr, os.ErrNotExist) {
		return finish(repairStatusAbsent, repairExitOK, nil)
	}

	rep, err := backendstore.Verify(cmd.Context(), dir)
	if err != nil {
		return finish("error", repairExitError, fmt.Errorf("verify %s: %w", dir, err))
	}
	fillRepairFindings(out, rep)
	switch {
	case rep.Clean():
		return finish(repairStatusClean, repairExitOK, nil)
	case dry && rep.Recoverable():
		return finish(repairStatusRecoverable, repairExitRepairable, nil)
	case dry:
		return finish(repairStatusRecoveryRequired, repairExitRecoveryRequired, nil)
	}

	// Only a safely recoverable registry is ever mutated, and only after an
	// explicit confirmation. Ambiguous findings fall through to Repair, which
	// writes its diagnosis report and refuses without touching the registry.
	if rep.Recoverable() && !yes {
		if !stdinIsInteractive(cmd.InOrStdin()) {
			out.NextAction = "nothing was changed. Re-run on a terminal to confirm, or pass --yes: `" + backendstore.RepairCommand + " --yes`"
			return finish(repairStatusCancelled, repairExitUnconfirmed, errors.New("confirmation required: not a terminal and --yes was not given"))
		}
		if !jsonOut {
			out.Status = repairStatusRecoverable
			renderRepairBackends(cmd.ErrOrStderr(), out)
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "Repair the backend registry at %s now? A verified backup is taken first. [y/N]: ", dir)
		line, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			out.NextAction = "nothing was changed. Re-run `" + backendstore.RepairCommand + "` and answer y, or pass --yes"
			return finish(repairStatusCancelled, repairExitUnconfirmed, errors.New("repair declined"))
		}
	}

	// Hold the data-dir lock for the repair so no daemon can start mid-write.
	lock, err := ownerlock.Acquire(cfg.DataDir, ownerlock.Info{
		Kind: ownerlock.KindCLI, Version: version, Command: strings.Join(os.Args, " ")})
	if err != nil {
		if errors.As(err, &oe) {
			return owned(oe)
		}
		return finish("error", repairExitError, err)
	}
	defer lock.Release()

	res, err := backendstore.Repair(cmd.Context(), dir, backendstore.Options{BackupDir: backupDir})
	var rre *backendstore.RecoveryRequiredError
	switch {
	case errors.As(err, &rre):
		out.ReportPath, out.BackupPath, out.BackupVerified = rre.ReportPath, rre.BackupPath, rre.BackupPath != ""
		return finish(repairStatusRecoveryRequired, repairExitRecoveryRequired, err)
	case errors.As(err, &oe):
		return owned(oe)
	case errors.Is(err, backendstore.ErrOwned):
		return owned(&ownerlock.OwnedError{Dir: cfg.DataDir})
	case err != nil:
		return finish("error", repairExitError, err)
	}
	if !res.Recovered { // became clean between verify and repair
		return finish(repairStatusClean, repairExitOK, nil)
	}
	out.Mutated = true
	out.ReportPath, out.BackupPath, out.BackupVerified = res.ReportPath, res.BackupPath, res.BackupPath != ""
	out.Discarded, out.ResolutionRule = res.Discarded, backendstore.ResolutionRule
	return finish(repairStatusRepaired, repairExitOK, nil)
}

// fillRepairFindings copies the Verify classification into the output model.
func fillRepairFindings(out *repairBackendsOutput, rep *backendstore.Report) {
	findings := map[string][]repairFinding{}
	verdicts := map[string]backendstore.Verdict{}
	for _, c := range rep.Collections {
		verdicts[c.Name] = c.Verdict
	}
	if rep.Integrity != nil {
		for _, f := range rep.Integrity.AllFindings() {
			if f.Severity == engine.SeverityInfo || verdicts[f.Collection] == backendstore.VerdictClean || f.Collection == "" {
				continue
			}
			findings[f.Collection] = append(findings[f.Collection], repairFinding{
				Severity: string(f.Severity), Code: string(f.Code), Classification: classificationOf(verdicts[f.Collection]),
				Segment: f.Location.Segment, Offset: f.Location.Offset, ID: f.Location.ID, Message: f.Message,
			})
		}
	}
	seen := map[string]bool{}
	for _, c := range rep.Collections {
		if c.Verdict == backendstore.VerdictClean {
			continue
		}
		risks := registryPreservation[c.Name]
		if risks == nil {
			risks = []string{}
		}
		rc := repairCollection{Name: c.Name, Verdict: string(c.Verdict), Classification: classificationOf(c.Verdict),
			Severities: c.Severities, Codes: c.Codes, Reasons: c.Reasons, StaleRevisions: len(c.Stale),
			PreservationRisks: risks, Findings: findings[c.Name]}
		if rc.Findings == nil {
			rc.Findings = []repairFinding{}
		}
		out.Collections = append(out.Collections, rc)
		for _, r := range risks {
			if !seen[r] {
				seen[r] = true
				out.PreservationRisks = append(out.PreservationRisks, r)
			}
		}
	}
	if rep.Recoverable() {
		out.ResolutionRule = backendstore.ResolutionRule
	}
}

func classificationOf(v backendstore.Verdict) string {
	if v == backendstore.VerdictRecoverable {
		return classSafelyRecoverable
	}
	return classRecoveryRequired
}

func ownerOf(oe *ownerlock.OwnedError) *repairOwner {
	o := &repairOwner{StopCommand: ownerStopCommand(oe.Owner)}
	if oe.Owner != nil {
		o.PID, o.Kind, o.Launch, o.Version, o.Addr = oe.Owner.PID, oe.Owner.Kind, oe.Owner.Launch, oe.Owner.Version, oe.Owner.Addr
	}
	return o
}

// ownerStopCommand is the exact command that stops the data-dir owner.
func ownerStopCommand(o *ownerlock.Owner) string {
	switch {
	case o != nil && o.Kind == ownerlock.KindCLI:
		return fmt.Sprintf("wait for warden CLI pid %d to finish", o.PID)
	case o != nil && o.Launch == ownerlock.LaunchSystemd:
		return daemonStopCommand
	case o != nil && runtime.GOOS == "darwin":
		return fmt.Sprintf("launchctl unload ~/Library/LaunchAgents/com.srajanpathak.warden.plist  (or, for a manually started daemon: kill %d)", o.PID)
	case o != nil:
		return fmt.Sprintf("kill %d", o.PID)
	}
	return daemonStopCommand + "  (or stop the manually started `warden daemon` process)"
}

const (
	daemonStopCommand  = "systemctl --user stop warden"
	daemonStartCommand = "systemctl --user start warden"
)

func repairNextAction(out *repairBackendsOutput, dry bool) string {
	switch out.Status {
	case repairStatusAbsent:
		return "none: there is no backend registry at " + out.Dir + " yet (the daemon creates it on first start)"
	case repairStatusClean:
		return "none: the backend registry is clean"
	case repairStatusOwned:
		who := "a running warden process"
		if o := out.Owner; o != nil && o.PID != 0 {
			who = fmt.Sprintf("%s pid %d", o.Kind, o.PID)
			if o.Launch == ownerlock.LaunchSystemd {
				who += " (systemd user service warden)"
			}
		}
		return fmt.Sprintf("nothing was read or changed: %s owns the data directory. Stop it with `%s`, re-run `%s`, then start it again (`%s`). Do not run a second `warden daemon` and do not delete the lock file",
			who, out.Owner.StopCommand, out.Command, daemonStartCommand)
	case repairStatusRecoverable:
		return "run `" + backendstore.RepairCommand + "` (backup-first; asks for confirmation), then start the daemon (`" + daemonStartCommand + "`)"
	case repairStatusRepaired:
		return "start the daemon (`" + daemonStartCommand + "`, or your service manager). Keep the backup until you have confirmed your tiers and default backend; to undo, stop the daemon and copy the collection directories from the backup over " + out.Dir
	case repairStatusRecoveryRequired:
		s := "the registry was left untouched: its history is ambiguous and is never resolved automatically. "
		if dry {
			s += "Run `" + backendstore.RepairCommand + "` to write the diagnosis report (it will not modify the registry). "
		} else if out.ReportPath != "" {
			s += "Review the report at " + out.ReportPath + ". "
		}
		return s + "Keep the daemon on the warden version that last opened this registry, or restore a known-good copy of " + out.Dir +
			" while the daemon is stopped, and attach the report to a bug report. Do not delete the registry: tiers, the default backend, models, role tiers, quotas and handover settings cannot be rebuilt by a rescan"
	}
	return "see the error above; nothing was changed unless a backup path is listed"
}

func printRepairBackends(w io.Writer, out *repairBackendsOutput, jsonOut bool) error {
	if jsonOut {
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	renderRepairBackends(w, out)
	return nil
}

// maxTextFindings caps the per-collection finding lines in text mode; --json
// always lists every finding.
const maxTextFindings = 10

func renderRepairBackends(w io.Writer, out *repairBackendsOutput) {
	mode := "repair"
	if out.Mode == "dry-run" {
		mode = "dry-run (read-only; nothing was changed)"
	}
	fmt.Fprintf(w, "backend registry: %s\nmode:   %s\nstatus: %s\n", out.Dir, mode, out.Status)
	if o := out.Owner; o != nil {
		if o.PID == 0 {
			fmt.Fprint(w, "owner:  unknown (the lock is held; owner metadata is unreadable)")
		} else {
			fmt.Fprintf(w, "owner:  %s pid %d", o.Kind, o.PID)
		}
		if o.Launch != "" {
			fmt.Fprintf(w, " (launched: %s)", o.Launch)
		}
		fmt.Fprintf(w, "\nstop:   %s\n", o.StopCommand)
	}
	for _, c := range out.Collections {
		label := "safely recoverable"
		if c.Classification == classRecoveryRequired {
			label = "AMBIGUOUS, recovery required"
		}
		fmt.Fprintf(w, "\ncollection %s: %s\n", c.Name, label)
		fmt.Fprintf(w, "  severity: %s   codes: %s\n", strings.Join(c.Severities, ","), strings.Join(c.Codes, ","))
		for _, r := range c.Reasons {
			fmt.Fprintf(w, "  reason: %s\n", r)
		}
		fmt.Fprintf(w, "  findings (%d):\n", len(c.Findings))
		for i, f := range c.Findings {
			if i == maxTextFindings {
				fmt.Fprintf(w, "    ... and %d more (--json lists all)\n", len(c.Findings)-i)
				break
			}
			loc := f.Segment
			if loc != "" {
				loc = fmt.Sprintf(" %s@%d", f.Segment, f.Offset)
			}
			fmt.Fprintf(w, "    [%s] %s%s: %s (%s)\n", f.Severity, f.Code, loc, f.Message, f.Classification)
		}
		if c.StaleRevisions > 0 {
			fmt.Fprintf(w, "  stale revisions to discard: %d (rule %s; each is listed in the report and kept in the backup)\n", c.StaleRevisions, backendstore.ResolutionRule)
		}
		if len(c.PreservationRisks) > 0 {
			fmt.Fprintf(w, "  could affect: %s\n", strings.Join(c.PreservationRisks, ", "))
		}
	}
	if len(out.Collections) > 0 {
		fmt.Fprintln(w)
	}
	if out.Mutated {
		fmt.Fprintf(w, "discarded stale revisions: %d\n", len(out.Discarded))
	}
	fmt.Fprintf(w, "report: %s\n", orNone(out.ReportPath, out.Mode == "dry-run"))
	backup := orNone(out.BackupPath, false)
	if out.BackupVerified {
		backup += " (verified: size and SHA-256 of every file)"
	}
	fmt.Fprintf(w, "backup: %s\n", backup)
	if out.Error != "" && out.Status != repairStatusOwned {
		fmt.Fprintf(w, "error:  %s\n", out.Error)
	}
	fmt.Fprintf(w, "next action: %s\n", out.NextAction)
}

func orNone(path string, dry bool) string {
	switch {
	case path != "":
		return path
	case dry:
		return "(none: --dry-run writes nothing; use --json to capture the findings)"
	}
	return "(none)"
}

// backendRecoveryMarker identifies a backend-registry recovery refusal in a
// daemon's startup output (it is part of RecoveryRequiredError's message).
var backendRecoveryMarker = "`" + backendstore.RepairCommand + "`"

var backendReportPathRE = regexp.MustCompile(`report: ([^;\n]+\.json)`)

// backendRecoverySteps is the operator procedure printed wherever warden
// refuses to open the backend registry (daemon startup, `warden update`).
func backendRecoverySteps(reportPath, backupPath string) string {
	var b strings.Builder
	b.WriteString("backend registry recovery (your tiers, default backend, models and settings are untouched):\n")
	fmt.Fprintf(&b, "  1. stop the daemon:   %s   (never run a second `warden daemon` beside the service)\n", daemonStopCommand)
	fmt.Fprintf(&b, "  2. inspect read-only: %s\n", backendstore.RepairDryRunCommand)
	fmt.Fprintf(&b, "  3. repair:            %s   (backup-first; asks for confirmation)\n", backendstore.RepairCommand)
	fmt.Fprintf(&b, "  4. start the daemon:  %s", daemonStartCommand)
	if reportPath != "" {
		b.WriteString("\n  report: " + reportPath)
	}
	if backupPath != "" {
		b.WriteString("\n  backup: " + backupPath)
	}
	return b.String()
}

// withBackendRecoverySteps appends the operator procedure to an error that
// reports a backend-registry recovery refusal, typed or (from a daemon's
// journal) textual. Other errors pass through unchanged.
func withBackendRecoverySteps(err error) error {
	if err == nil {
		return nil
	}
	var rre *backendstore.RecoveryRequiredError
	if errors.As(err, &rre) {
		return fmt.Errorf("%w\n%s", err, backendRecoverySteps(rre.ReportPath, rre.BackupPath))
	}
	msg := err.Error()
	if !strings.Contains(msg, backendRecoveryMarker) || strings.Contains(msg, "backend registry recovery (") {
		return err
	}
	report := ""
	if m := backendReportPathRE.FindStringSubmatch(msg); m != nil {
		report = strings.TrimSpace(m[1])
	}
	return fmt.Errorf("%w\n%s", err, backendRecoverySteps(report, ""))
}
