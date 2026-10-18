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

When the new release moves the data to a newer schema (the schema-2 cutover to
the single warden-db), the daemon is stopped first, a data snapshot is taken,
and the NEW binary runs "migrate --apply" (strict preflight, an immutable
checksummed backup, staged export with count/hash/reference verification, atomic
promotion, then the ledger). The daemon is restarted only afterwards and must
report the new schema. If the migration or the restart fails, the previous
binary and data are restored (a promoted warden-db is preserved under backups/,
never deleted). Legacy stores are kept after the cutover.

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
				// Runs after the swap and before the restart. This (old) binary only
				// reconciles config itself; data migrations are run BY THE NEW
				// BINARY (`<new> migrate --apply`), because only it knows the target
				// layout. See runUpdateMigrations.
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

// Seams for the update data-migration step (replaced in tests, which must never
// execute a real binary against real data).
var (
	targetBinarySchema = binarySchemaVersion
	runTargetMigrate   = execTargetMigrate
)

// runUpdateMigrations is the updater's Migrate hook. The updater is the OLD
// binary; it reconciles config, then hands the data migration to the freshly
// installed binary (`<new> migrate --apply`), which owns the journaled schema-2
// cutover (strict preflight, immutable verified backup, staging + equivalence
// proof, atomic promotion, ledger stamp). It runs only when the new binary
// writes a newer schema than the ledger records. The daemon is already stopped
// (the updater stops it before snapshotting); if something still owns the data
// dir the migration refuses and the updater rolls back. Any failure — including
// a ledger that does not end up at the target schema, clean — returns an error,
// which the updater turns into a binary + data rollback. It never starts the new
// daemon on unmigrated data: that is the next updater step and the daemon's
// boot guard refuses it independently.
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
			return nil // nothing on disk to migrate; the new daemon stamps it
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

// execTargetMigrate runs `<bin> migrate --apply --data-dir <dir>`.
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
		if pe.RepairCommand != "" && !strings.Contains(pe.RepairCommand, "repair backends") {
			return err
		}
		return fmt.Errorf("%w\n%s", err, backendRecoverySteps("", ""))
	}
	return withBackendRecoverySteps(err)
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
	out, err := cmd.CombinedOutput()
	if err != nil {
		// If command output contains valid JSON report, decode it to extract findings
		var rep migrateCheckReport
		if jerr := json.Unmarshal(out, &rep); jerr == nil && len(rep.Findings) > 0 {
			return findingsToPreflightResult(rep.Findings), nil
		}
		// If binary doesn't support migrate subcommand (older binary), fall back to in-process check
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && strings.Contains(string(out), "unknown command") {
			return wholeStorePreflight(ctx, dataDir)
		}
		// Otherwise report failure
		return updater.PreflightResult{
			Blockers: []string{fmt.Sprintf("%s migrate --check: %v (%s)", targetBin, err, strings.TrimSpace(string(out)))},
		}, nil
	}

	var rep migrateCheckReport
	if jerr := json.Unmarshal(out, &rep); jerr != nil {
		// Output wasn't JSON or check produced text; fall back
		return wholeStorePreflight(ctx, dataDir)
	}

	return findingsToPreflightResult(rep.Findings), nil
}

func findingsToPreflightResult(findings []migrate.Finding) updater.PreflightResult {
	res := updater.PreflightResult{}
	repairCommands := make(map[string]bool)

	for _, f := range findings {
		// The cutover-pending marker only exists to stop updaters that cannot run
		// the cutover; this updater runs it (runUpdateMigrations), so it is a note.
		if f.Class == migrate.ClassSchema2CutoverPending {
			res.Notes = append(res.Notes, fmt.Sprintf("%s: schema-2 cutover will run after the binary swap", f.Store))
			continue
		}
		switch f.Severity {
		case migrate.SeverityAuto:
			res.Notes = append(res.Notes, fmt.Sprintf("%s: %s", f.Store, f.Message))
		case migrate.SeverityRepairable, migrate.SeverityBlocking:
			res.Blockers = append(res.Blockers, fmt.Sprintf("%s: %s", f.Store, f.Message))
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
