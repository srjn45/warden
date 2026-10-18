package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/srjn45/warden/internal/audit"
	"github.com/srjn45/warden/internal/backendstore"
	"github.com/srjn45/warden/internal/config"
	"github.com/srjn45/warden/internal/migrate"
	"github.com/srjn45/warden/internal/schema"
	"github.com/srjn45/warden/internal/updater"
)

func newUpdateCmd() *cobra.Command {
	var (
		checkOnly bool
		planOnly  bool
		yes       bool
		force     bool
		pin       string
		ready     time.Duration
	)
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update the installed warden binary from GitHub Releases",
		Long: `Download a verified GitHub release archive, atomically replace
~/.local/bin/warden, re-sign on macOS when the warden-codesign identity is
present, run config migrations, restart the user-level daemon service, and
wait for /healthz to report ok on the new version AND on the data schema
version the new binary writes (see "warden version").

The update is a transaction. Before any change it records the current binary,
service manager and daemon version, and verifies the backend store read-only:
an unrecoverable store stops the update with the diagnosis and repair command
instead of swapping into a daemon that cannot boot (auto-recoverable findings
are reported, not blocking). After the restart the real daemon startup error
(journal / stderr tail) is shown on failure. On ANY failure the previous binary
is restored, the service restarted, and the old version verified healthy; both
the original failure and the rollback outcome are reported.

Flags:
  --check            report whether an update is available without applying it
  --plan             inspect the planned upgrade path, breaking changes, downtime, and disk needed
  --yes              skip confirmation for breaking or data-touching updates
  --version <tag>    install a specific release (e.g. 9.9.0 or v9.9.0)
  --force            reinstall even when already on the target version
  --ready-timeout    overall deadline for the daemon to become healthy (default 90s)

Examples:
  warden update
  warden update --check
  warden update --plan
  warden update --version v9.9.0
  wd update --force`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfgPath := configPathFor(cmd)
			cfg := config.Load(cfgPath)
			healthURL := "http://" + strings.TrimSpace(cfg.Addr) + "/healthz"
			if strings.TrimSpace(cfg.Addr) == "" {
				healthURL = updater.DefaultHealthURL
			}
			var installBin string
			if home, err := os.UserHomeDir(); err == nil && home != "" {
				installBin = filepath.Join(home, ".local", "bin", "warden")
			}

			opts := updater.Options{
				CurrentVersion: version,
				TargetVersion:  pin,
				Force:          force,
				CheckOnly:      checkOnly,
				HealthURL:      healthURL,
				ReadyTimeout:   ready,
				DataDir:        cfg.DataDir,
				Context:        cmd.Context(),
				Preflight: func(ctx context.Context) (updater.PreflightResult, error) {
					return wholeStorePreflight(ctx, cfg.DataDir)
				},
				TargetPreflight: func(ctx context.Context, targetBin string) (updater.PreflightResult, error) {
					return targetBinaryPreflight(ctx, targetBin, cfg.DataDir)
				},
				Stdout: cmd.OutOrStdout(),
				Migrate: func() error {
					return runUpdateMigrations(cmd.Context(), cfgPath, cfg.DataDir, installBin, cmd.OutOrStdout())
				},
				TargetSchema:  binarySchemaVersion,
				CurrentSchema: schema.SchemaVersion,
			}
			if home, err := os.UserHomeDir(); err == nil && home != "" {
				opts.StagingDir = filepath.Join(home, ".warden", "tmp")
				opts.InstallBin = installBin
			}

			if planOnly {
				p, err := updater.BuildPlan(opts)
				if err != nil {
					return err
				}
				fmt.Fprint(cmd.OutOrStdout(), updater.FormatPlanText(p))
				return nil
			}

			if checkOnly {
				res, err := updater.Check(opts)
				_ = res
				return updateRecoveryGuidance(err)
			}

			// Pre-evaluate plan to check if confirmation is required for breaking/data-touching updates
			if !yes {
				if p, err := updater.BuildPlan(opts); err == nil && p.RequiresConfirm {
					fmt.Fprint(cmd.OutOrStdout(), updater.FormatPlanText(p))
					ok, err := confirmOrYes(cmd, false, "Proceed with update? [y/N]: ")
					if err != nil {
						return err
					}
					if !ok {
						fmt.Fprintln(cmd.OutOrStdout(), "update cancelled")
						return nil
					}
				}
			}

			res, err := updater.Apply(opts)
			_ = res
			return updateRecoveryGuidance(err)
		},
	}
	cmd.Flags().BoolVar(&checkOnly, "check", false, "query and print whether an update is available without applying it")
	cmd.Flags().BoolVar(&planOnly, "plan", false, "inspect planned upgrade path, breaking changes, downtime, and disk needed without changing anything")
	cmd.Flags().BoolVar(&yes, "yes", false, "automatically confirm updates that require confirmation")
	cmd.Flags().BoolVar(&force, "force", false, "reinstall even when already on the target version")
	cmd.Flags().DurationVar(&ready, "ready-timeout", updater.DefaultReadyTimeout, "overall deadline for the restarted daemon to report healthy on the new version")
	cmd.Flags().StringVar(&pin, "version", "", "install a specific release tag (e.g. 9.9.0 or v9.9.0)")
	return cmd
}

// binarySchemaVersion asks an installed warden binary which data schema it
// writes (`warden version --json`). The running updater is the OLD binary, so
// the target's schema can only come from the target itself. A release that
// predates the schema ledger has no such field and reports 0, which is also
// what its daemon's /healthz reports — the readiness comparison stays exact.
func binarySchemaVersion(ctx context.Context, bin string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "version", "--json").Output()
	if err != nil {
		return 0, fmt.Errorf("%s version --json: %w", bin, err)
	}
	var bi struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err := json.Unmarshal(out, &bi); err != nil {
		return 0, fmt.Errorf("decode %s version --json: %w", bin, err)
	}
	return bi.SchemaVersion, nil
}

// Seams for the update data-migration step. Tests replace these so they never
// execute a binary against anything except fixture data.
var (
	targetBinarySchema = binarySchemaVersion
	runTargetMigrate   = execTargetMigrate
)

// runUpdateMigrations is called after the old daemon has stopped and the new
// binary has been installed. Config reconciliation remains first; when the new
// binary writes a newer schema, that binary owns the journaled migration.
func runUpdateMigrations(ctx context.Context, cfgPath, dataDir, installBin string, out io.Writer) error {
	if err := config.Reconcile(cfgPath); err != nil {
		return err
	}
	if dataDir == "" || installBin == "" {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	target, err := targetBinarySchema(ctx, installBin)
	if err != nil {
		return fmt.Errorf("read the new binary's data schema: %w", err)
	}
	current := 0
	switch l, err := schema.Load(dataDir); {
	case err == nil:
		current = l.SchemaVersion
		if l.MigrationInterrupted() {
			return fmt.Errorf("a previous migration (%s) was interrupted; run `warden migrate --resume` or `--restore` before updating", l.InProgress.Migration)
		}
	case errors.Is(err, schema.ErrNoLedger):
		if schema.InferBaseline(dataDir).Fresh {
			return nil
		}
	default:
		return fmt.Errorf("read schema ledger: %w", err)
	}
	if target == 0 || current >= target {
		return nil
	}
	fmt.Fprintf(out, "migrating data schema %d → %d with the new binary…\n", current, target)
	if err := runTargetMigrate(ctx, installBin, dataDir, out); err != nil {
		return fmt.Errorf("data migration to schema %d failed (the previous binary and data are restored): %w", target, err)
	}
	l, err := schema.Load(dataDir)
	if err != nil {
		return fmt.Errorf("verify migrated ledger: %w", err)
	}
	if l.SchemaVersion != target || l.MigrationInterrupted() {
		return fmt.Errorf("migration finished but the ledger is at schema %d (interrupted=%v), want %d", l.SchemaVersion, l.MigrationInterrupted(), target)
	}
	return nil
}

// execTargetMigrate runs the newly installed binary's migration while the
// service is still stopped. Its output is retained in the updater diagnostics.
func execTargetMigrate(ctx context.Context, bin, dataDir string, out io.Writer) error {
	cmd := exec.CommandContext(ctx, bin, "migrate", "--apply", "--data-dir", dataDir)
	var buf bytes.Buffer
	cmd.Stdout = io.MultiWriter(out, &buf)
	cmd.Stderr = io.MultiWriter(out, &buf)
	if err := cmd.Run(); err != nil {
		tail := strings.TrimSpace(buf.String())
		if len(tail) > 2000 {
			tail = tail[len(tail)-2000:]
		}
		return fmt.Errorf("%s migrate --apply: %w\n%s", bin, err, tail)
	}
	return nil
}

// updateRecoveryGuidance appends the backend-registry recovery procedure
// (exact command + report location) to an update failure caused by the
// registry: the preflight blocker, or a new daemon that refused to start on it
// (recognised in the captured journal/stderr tail). Other errors are unchanged.
func updateRecoveryGuidance(err error) error {
	var pe *updater.PreflightError
	if errors.As(err, &pe) {
		if isBackendPreflightFailure(pe) {
			return fmt.Errorf("%w\n%s", err, backendRecoverySteps("", ""))
		}
		return err
	}
	return withBackendRecoverySteps(err)
}

// isBackendPreflightFailure only examines the structured fields produced by
// findingsToPreflightResult. In particular, it never searches arbitrary command
// output: a JSON report can mention backends while describing an unrelated
// failure, and must not cause backend-specific recovery advice.
func isBackendPreflightFailure(pe *updater.PreflightError) bool {
	if pe.RepairCommand == backendstore.RepairCommand {
		return true
	}
	if len(pe.Blockers) == 0 {
		return false
	}
	for _, blocker := range pe.Blockers {
		if !isBackendBlocker(blocker) {
			return false
		}
	}
	return true
}

func isBackendBlocker(blocker string) bool {
	blocker = strings.TrimSpace(blocker)
	if blocker == "" || strings.ContainsAny(blocker, "\r\n") {
		return false
	}
	pathPart, _, found := strings.Cut(blocker, ":")
	if !found {
		return false
	}
	pathPart = strings.TrimSpace(pathPart)
	if pathPart == "backends" {
		return true
	}
	for _, segment := range strings.Split(filepath.ToSlash(pathPart), "/") {
		if segment == "backends" {
			return true
		}
	}
	return false
}

// wholeStorePreflight runs read-only verification across all ScrivaDB stores.
func wholeStorePreflight(ctx context.Context, dataDir string) (updater.PreflightResult, error) {
	runner := migrate.NewRunner(nil)
	findings, err := runner.Check(migrate.Env{DataDir: dataDir, Context: ctx}, schema.SchemaVersion)
	if err != nil {
		return updater.PreflightResult{
			Blockers: []string{fmt.Sprintf("verification failed: %v", err)},
		}, nil
	}
	return findingsToPreflightResult(findings), nil
}

// targetBinaryPreflight runs `targetBin migrate --check --data-dir <dataDir> --json`.
func targetBinaryPreflight(ctx context.Context, targetBin, dataDir string) (updater.PreflightResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, targetBin, "migrate", "--check", "--data-dir", dataDir, "--json")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	var rep migrateCheckReport
	if jerr := json.Unmarshal(stdout.Bytes(), &rep); jerr == nil {
		// `migrate --check` intentionally exits nonzero when it reports findings.
		// Its report is always stdout; Cobra's human-readable error is stderr.
		return findingsToPreflightResult(rep.Findings), nil
	}
	return updater.PreflightResult{Blockers: []string{targetPreflightDiagnostic(targetBin, err, stderr.String())}}, nil
}

func targetPreflightDiagnostic(targetBin string, runErr error, stderr string) string {
	const maxStderr = 1000
	stderr = strings.TrimSpace(audit.RedactSecrets(stderr))
	if len(stderr) > maxStderr {
		stderr = stderr[len(stderr)-maxStderr:] + " …(truncated)"
	}
	if stderr == "" {
		return fmt.Sprintf("%s migrate --check failed: %v (no stderr)", targetBin, runErr)
	}
	return fmt.Sprintf("%s migrate --check failed: %v (stderr: %s)", targetBin, runErr, stderr)
}

func findingsToPreflightResult(findings []migrate.Finding) updater.PreflightResult {
	res := updater.PreflightResult{}
	repairCommands := make(map[string]bool)

	for _, f := range findings {
		entry := f.Message
		if f.Store != "" && !strings.HasPrefix(entry, f.Store) {
			entry = fmt.Sprintf("%s: %s", f.Store, entry)
		}
		// The cutover-pending marker only stops updaters that cannot run the
		// cutover; this updater runs it after the binary swap, so it is a note.
		if f.Class == migrate.ClassSchema2CutoverPending {
			res.Notes = append(res.Notes, fmt.Sprintf("%s: schema-2 cutover will run after the binary swap", f.Store))
			continue
		}
		switch f.Severity {
		case migrate.SeverityAuto:
			res.Notes = append(res.Notes, entry)
		case migrate.SeverityRepairable, migrate.SeverityBlocking:
			// Repairable with no manual command is preserved/quarantined by the
			// target migration itself: a note, not a blocker.
			if f.Severity == migrate.SeverityRepairable && f.Command == "" {
				res.Notes = append(res.Notes, entry)
				continue
			}
			res.Blockers = append(res.Blockers, entry)
			if f.Command != "" {
				repairCommands[f.Command] = true
			}
		}
	}

	if len(repairCommands) == 1 {
		for cmd := range repairCommands {
			res.RepairCommand = cmd
		}
	} else if len(repairCommands) > 1 {
		res.RepairCommand = "warden repair all --resolve-history=live-wins"
	}

	return res
}

// backendPreflight is the read-only pre-swap check of the backend registry.
// Ambiguous or unreadable damage blocks the update; recoverable damage is
// reported only (the new daemon repairs it itself on boot).
func backendPreflight(ctx context.Context, dir string) (updater.PreflightResult, error) {
	res := updater.PreflightResult{RepairCommand: backendstore.RepairCommand}
	rep, err := backendstore.Verify(ctx, dir)
	if err != nil {
		res.Blockers = append(res.Blockers, fmt.Sprintf("%s: verification failed: %v", dir, err))
		return res, nil
	}
	// Index lag beside the running daemon is expected, not something the new
	// daemon has to repair — keep it out of the update notes.
	lag := rep.LiveIndexLag()
	for _, c := range rep.Collections {
		switch c.Verdict {
		case backendstore.VerdictRecoverable:
			if lag {
				continue
			}
			res.Notes = append(res.Notes, fmt.Sprintf("%s: %s", c.Name, strings.Join(c.Codes, ", ")))
		case backendstore.VerdictAmbiguous:
			why := strings.Join(c.Reasons, "; ")
			res.Blockers = append(res.Blockers, fmt.Sprintf("%s: %s (%s)", c.Name, strings.Join(c.Codes, ", "), why))
		}
	}
	return res, nil
}
