package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/srjn45/warden/internal/config"
	"github.com/srjn45/warden/internal/migrate"
	"github.com/srjn45/warden/internal/migrate/schema3"
	"github.com/srjn45/warden/internal/ownerlock"
	"github.com/srjn45/warden/internal/schema"
)

type migrateCheckReport struct {
	DataDir        string            `json:"data_dir"`
	CurrentVersion int               `json:"current_version"`
	TargetVersion  int               `json:"target_version"`
	Clean          bool              `json:"clean"`
	Findings       []migrate.Finding `json:"findings"`
}

func newMigrateCmd() *cobra.Command {
	var (
		checkFlag      bool
		planFlag       bool
		retirementFlag bool
		cleanupFlag    bool
		dryRunFlag     bool
		yesFlag        bool
		stagingFlag    bool
		applyFlag      bool
		resumeFlag     bool
		restoreFlag    bool
		jsonFlag       bool
		targetVer      int
		dataDirFlag    string
		stagingDir     string
	)

	cmd := &cobra.Command{
		Use:           "migrate",
		Short:         "Inspect, apply, resume, or restore data format migrations",
		SilenceUsage:  true,
		SilenceErrors: true,
		Long: `Run data format migrations for the Warden data directory.

Actions (select one):
  --check    Read-only preflight check across all stores and pending migrations,
             including a preview of the schema-2 mapping (pipeline references,
             legacy pipelines/*.json copies, relationships) on a private copy,
             so a blocker is reported before anything is stopped
  --plan     Read-only schema-2 (warden-db) cutover plan: source/target inventory,
             strict verification of every legacy store (corruption, ambiguity,
             duplicate identities, unsupported layouts, unsafe artifacts),
             backup location and blocking conditions. Writes nothing
  --staging-export
             Export normalized schema-2 staging collections into a separate
             review directory. Reconciles pipelines-db with the legacy
             pipelines/*.json copies (identical copies dedupe, deleted pipelines
             are not resurrected and keep a historical_pipeline provenance on
             archived agents/plans; conflicts, unproven deletions and live
             records of a missing pipeline refuse). Refuses relationship
             violations and divergent existing staging records. Never writes
             legacy sources or schema.json
  --apply    Apply pending migrations up to the target schema version. For a
             schema-1 data dir this is the schema-2 cutover to the single
             warden-db: the daemon must be stopped; strict preflight, then an
             immutable checksummed backup under backups/pre-schema2-*, staging
             export, counts/hash/reference verification, atomic promotion, and
             only then the ledger moves to schema 2. Legacy stores are kept
             (nothing is deleted). Any refusal before promotion leaves the data
             exactly as it was
  --retirement
             Read-only legacy-store retirement entry-criteria report: schema-2
             active, warden-db present, backup verified, migration succeeded,
             no unresolved failures or rollbacks, zero legacy opens, runtime
             observed, support window elapsed. Deletes nothing
  --cleanup-legacy
             Explicit, backup-gated removal of the retained schema-1 stores. Re-runs
             the --retirement gate, verifies warden-db and re-verifies the cutover
             backup against its MANIFEST, lists the exact paths, asks for
             confirmation (or --yes), then MOVES them to
             backups/legacy-quarantine-<time>/ (never deletes) and records an audit
             event. --dry-run only reports. Refuses on any unmet criterion;
             re-running when nothing remains is a no-op. Recover by moving paths
             back from the quarantine or restoring the pre-schema2 backup
  --resume   Resume an interrupted migration recorded in the ledger journal
  --restore  Restore from pre-migration snapshot after an interrupted migration
             (a promoted warden-db is preserved under backups/, never deleted)

Older binaries refuse a schema-2 data dir before opening any store; there is no
automatic downgrade. "wd update" runs the cutover for you (via the new binary)
and rolls the binary and data back if it fails.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Validate mutually exclusive flags
			count := 0
			if checkFlag {
				count++
			}
			if planFlag {
				count++
			}
			if stagingFlag {
				count++
			}
			if applyFlag {
				count++
			}
			if resumeFlag {
				count++
			}
			if retirementFlag {
				count++
			}
			if restoreFlag {
				count++
			}
			if cleanupFlag {
				count++
			}

			if count > 1 {
				return errors.New("cannot specify more than one of --check, --plan, --retirement, --cleanup-legacy, --apply, --resume, --restore")
			}
			if count == 0 {
				checkFlag = true // Default action is read-only check
			}

			dataDir := dataDirFlag
			if dataDir == "" {
				cfg := config.Load(configPathFor(cmd))
				dataDir = cfg.DataDir
			}

			if targetVer <= 0 {
				targetVer = schema.SchemaVersion
			}

			env := migrate.Env{
				DataDir:       dataDir,
				BinaryVersion: version,
				Context:       cmd.Context(),
			}
			runner := migrate.NewRunner(nil)
			if targetVer == 3 {
				if checkFlag || planFlag {
					return runSchema3Plan(cmd, dataDir, jsonFlag)
				}
				return migrate.ErrUpdaterTransactionRequired
			}

			switch {
			case retirementFlag:
				rep, err := migrate.EvaluateRetirement(dataDir, migrate.RetirementOptions{})
				if err != nil {
					return err
				}
				if jsonFlag {
					enc := json.NewEncoder(cmd.OutOrStdout())
					enc.SetIndent("", "  ")
					return enc.Encode(rep)
				}
				fmt.Fprint(cmd.OutOrStdout(), rep.Summary())
				return nil
			case cleanupFlag:
				return runLegacyCleanup(cmd, dataDir, dryRunFlag, yesFlag, jsonFlag)
			case planFlag:
				return runSchema2Plan(cmd, dataDir, jsonFlag)

			case stagingFlag:
				return runSchema2StagingExport(cmd, dataDir, stagingDir, jsonFlag)

			case checkFlag:
				findings, err := runner.Check(env, targetVer)
				if err != nil {
					return fmt.Errorf("migrate: check failed: %w", err)
				}

				currentVer := 0
				if l, lerr := schema.Load(dataDir); lerr == nil {
					currentVer = l.SchemaVersion
				}

				var blockers, repairable []migrate.Finding
				repairCommands := make(map[string]bool)
				for _, f := range findings {
					switch f.Severity {
					case migrate.SeverityRepairable:
						repairable = append(repairable, f)
						if f.Command != "" {
							repairCommands[f.Command] = true
						}
					case migrate.SeverityBlocking:
						blockers = append(blockers, f)
						if f.Command != "" {
							repairCommands[f.Command] = true
						}
					}
				}

				isClean := len(blockers) == 0 && len(repairable) == 0

				if jsonFlag {
					rep := migrateCheckReport{
						DataDir:        dataDir,
						CurrentVersion: currentVer,
						TargetVersion:  targetVer,
						Clean:          isClean,
						Findings:       findings,
					}
					enc, err := json.MarshalIndent(rep, "", "  ")
					if err != nil {
						return err
					}
					fmt.Fprintln(cmd.OutOrStdout(), string(enc))
					if !isClean {
						return fmt.Errorf("migrate: check found %d issue(s) requiring repair", len(blockers)+len(repairable))
					}
					return nil
				}

				out := cmd.OutOrStdout()
				if len(findings) == 0 {
					fmt.Fprintf(out, "all stores clean (schema version %d)\n", currentVer)
					return nil
				}

				fmt.Fprintf(out, "preflight findings for %s (current: schema %d, target: schema %d):\n", dataDir, currentVer, targetVer)
				for _, f := range findings {
					switch f.Severity {
					case migrate.SeverityAuto:
						fmt.Fprintf(out, "  [AUTO] %s: %s\n", f.Store, f.Message)
					case migrate.SeverityRepairable:
						fmt.Fprintf(out, "  [REPAIRABLE] %s: %s\n", f.Store, f.Message)
					case migrate.SeverityBlocking:
						fmt.Fprintf(out, "  [BLOCKING] %s: %s\n", f.Store, f.Message)
					}
				}

				if len(repairCommands) > 0 {
					fmt.Fprintln(out, "\nsuggested repair commands:")
					for cmdStr := range repairCommands {
						fmt.Fprintf(out, "  %s\n", cmdStr)
					}
				}

				if !isClean {
					return fmt.Errorf("migrate: check found %d issue(s) requiring repair", len(blockers)+len(repairable))
				}
				return nil

			case applyFlag:
				lock, err := ownerlock.Acquire(dataDir, ownerlock.Info{
					Kind:    ownerlock.KindCLI,
					Version: version,
					Command: strings.Join(os.Args, " "),
				})
				if err != nil {
					return fmt.Errorf("cannot apply migrations while daemon is running: %w", err)
				}
				defer lock.Release()

				if err := runner.Apply(env, targetVer); err != nil {
					return fmt.Errorf("migrate: apply failed: %w", err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "migrated %s to schema version %d\n", dataDir, targetVer)
				return nil

			case resumeFlag:
				lock, err := ownerlock.Acquire(dataDir, ownerlock.Info{
					Kind:    ownerlock.KindCLI,
					Version: version,
					Command: strings.Join(os.Args, " "),
				})
				if err != nil {
					return fmt.Errorf("cannot resume migration while daemon is running: %w", err)
				}
				defer lock.Release()

				if err := runner.Resume(env); err != nil {
					return fmt.Errorf("migrate: resume failed: %w", err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "resumed and completed interrupted migration for %s\n", dataDir)
				return nil

			case restoreFlag:
				lock, err := ownerlock.Acquire(dataDir, ownerlock.Info{
					Kind:    ownerlock.KindCLI,
					Version: version,
					Command: strings.Join(os.Args, " "),
				})
				if err != nil {
					return fmt.Errorf("cannot restore migration while daemon is running: %w", err)
				}
				defer lock.Release()

				if err := runner.Restore(env); err != nil {
					return fmt.Errorf("migrate: restore failed: %w", err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "restored pre-migration state for %s\n", dataDir)
				return nil
			}

			return nil
		},
	}

	cmd.Flags().BoolVar(&checkFlag, "check", false, "run read-only preflight check across all stores and pending migrations")
	cmd.Flags().BoolVar(&planFlag, "plan", false, "show the read-only schema-2 (warden-db) cutover plan and what blocks it")
	cmd.Flags().BoolVar(&stagingFlag, "staging-export", false, "export normalized schema-2 staging collections without touching legacy sources")
	cmd.Flags().BoolVar(&retirementFlag, "retirement", false, "report legacy-store retirement entry criteria (read-only; deletes nothing)")
	cmd.Flags().BoolVar(&cleanupFlag, "cleanup-legacy", false, "move retired schema-1 stores to a quarantine dir after verifying warden-db and the cutover backup (needs confirmation)")
	cmd.Flags().BoolVar(&dryRunFlag, "dry-run", false, "with --cleanup-legacy: report what would move; change nothing")
	cmd.Flags().BoolVar(&yesFlag, "yes", false, "with --cleanup-legacy: confirm without prompting")
	cmd.Flags().BoolVar(&applyFlag, "apply", false, "apply pending migrations up to the target schema version")
	cmd.Flags().BoolVar(&resumeFlag, "resume", false, "resume an interrupted migration")
	cmd.Flags().BoolVar(&restoreFlag, "restore", false, "restore from pre-migration snapshot after an interrupted migration")
	cmd.Flags().BoolVar(&jsonFlag, "json", false, "output results in JSON format")
	cmd.Flags().IntVar(&targetVer, "target", schema.SchemaVersion, "target schema version to migrate to")
	cmd.Flags().StringVar(&dataDirFlag, "data-dir", "", "path to the Warden data directory")
	cmd.Flags().StringVar(&stagingDir, "staging-dir", "", "target directory for --staging-export (default: <data-dir>/warden-db-staging)")

	return cmd
}

// runLegacyCleanup previews the cleanup (a dry run through the same gate),
// asks for confirmation, then performs it.
func runLegacyCleanup(cmd *cobra.Command, dataDir string, dryRun, yes, asJSON bool) error {
	out := cmd.OutOrStdout()
	opt := migrate.CleanupOptions{DryRun: true, BinaryVersion: version, Actor: "cli"}
	rep, err := migrate.CleanupLegacy(cmd.Context(), dataDir, opt)
	if err != nil {
		if rep != nil && rep.Retirement != nil && !asJSON {
			fmt.Fprint(out, rep.Retirement.Summary())
		}
		return err
	}
	if !dryRun && len(rep.Paths) > 0 {
		if !asJSON {
			fmt.Fprint(out, rep.Summary())
		}
		prompt := fmt.Sprintf("Move %d legacy path(s) to quarantine? Backup %s was verified. [y/N] ", len(rep.Paths), rep.Backup)
		ok, cerr := confirmOrYes(cmd, yes, prompt)
		if cerr != nil {
			return cerr
		}
		if !ok {
			return migrate.ErrCleanupUnconfirmed
		}
		opt.DryRun, opt.Confirm = false, true
		if rep, err = migrate.CleanupLegacy(cmd.Context(), dataDir, opt); err != nil {
			return err
		}
	}
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	fmt.Fprint(out, rep.Summary())
	return nil
}

func runSchema2StagingExport(cmd *cobra.Command, dataDir, targetDir string, asJSON bool) error {
	rep, err := migrate.ExportSchema2Staging(cmd.Context(), dataDir, migrate.StagingExportOptions{
		TargetDir: targetDir,
		Now:       time.Now(),
	})
	out := cmd.OutOrStdout()
	if asJSON {
		enc, jerr := json.MarshalIndent(rep, "", "  ")
		if jerr != nil {
			return jerr
		}
		fmt.Fprintln(out, string(enc))
		if err != nil {
			return err
		}
		return nil
	}
	if rep != nil {
		fmt.Fprintf(out, "schema-2 staging export for %s -> %s\n", rep.DataDir, rep.TargetDir)
		for _, name := range sortedStringKeys(rep.Records) {
			fmt.Fprintf(out, "  %s: %d record(s)\n", name, rep.Records[name])
		}
		for _, f := range rep.Findings {
			tag := map[migrate.Severity]string{migrate.SeverityBlocking: "BLOCKING", migrate.SeverityRepairable: "NOTE", migrate.SeverityAuto: "INFO"}[f.Severity]
			if tag == "" {
				tag = string(f.Severity)
			}
			if f.Store != "" {
				fmt.Fprintf(out, "  [%s] %s: %s\n", tag, f.Store, f.Message)
			} else {
				fmt.Fprintf(out, "  [%s] %s\n", tag, f.Message)
			}
		}
	}
	return err
}

func sortedStringKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// runSchema2Plan prints the read-only schema-2 cutover plan. It acquires no
// lock and writes nothing; a blocked plan exits non-zero so scripts can gate.
func runSchema2Plan(cmd *cobra.Command, dataDir string, asJSON bool) error {
	plan, err := migrate.PreflightSchema2(cmd.Context(), dataDir, time.Now())
	if err != nil {
		return fmt.Errorf("migrate: plan failed: %w", err)
	}
	out := cmd.OutOrStdout()
	if asJSON {
		enc, err := json.MarshalIndent(plan, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(out, string(enc))
	} else {
		fmt.Fprintf(out, "schema-2 cutover plan for %s (schema %d -> %d)\n", plan.DataDir, plan.From, plan.To)
		fmt.Fprintf(out, "  target:  %s (collections: %d, exists: %v)\n", plan.Target.Dir, len(plan.Target.Collections), plan.Target.Exists)
		fmt.Fprintf(out, "  backup:  %s (created exclusively, never reused, read-only)\n", plan.BackupDir)
		fmt.Fprintln(out, "  rules:   daemon must be stopped; no online or dual-writer migration;")
		fmt.Fprintln(out, "           nothing is overwritten, merged, salvaged or deleted; legacy data stays intact until you remove it.")
		fmt.Fprintln(out, "\nsources:")
		for _, e := range plan.Sources {
			if !e.Present {
				continue
			}
			fmt.Fprintf(out, "  %-12s %-28s %5d files %10d bytes", e.Disposition, e.Path, e.Files, e.Bytes)
			if e.Destination != "" {
				fmt.Fprintf(out, "  -> %s", e.Destination)
			}
			fmt.Fprintln(out)
		}
		if len(plan.Findings) > 0 {
			fmt.Fprintln(out, "\nfindings:")
			for _, f := range plan.Findings {
				tag := map[migrate.Severity]string{migrate.SeverityBlocking: "BLOCKING", migrate.SeverityRepairable: "NOTE", migrate.SeverityAuto: "INFO"}[f.Severity]
				if f.Class != "" {
					tag += ":" + f.Class
				}
				fmt.Fprintf(out, "  [%s] %s: %s\n", tag, f.Store, f.Message)
				if f.Details != "" {
					for _, ln := range strings.Split(f.Details, "\n") {
						fmt.Fprintf(out, "      %s\n", ln)
					}
				}
			}
		}
		if plan.Blocked {
			fmt.Fprintln(out, "\nresult: BLOCKED — nothing was changed")
		} else {
			fmt.Fprintln(out, "\nresult: ready — nothing was changed (this command is read-only)")
		}
	}
	if plan.Blocked {
		return errors.New("migrate: schema-2 cutover is blocked")
	}
	return nil
}

func runSchema3Plan(cmd *cobra.Command, dataDir string, asJSON bool) error {
	report, err := schema3.Plan(cmd.Context(), dataDir)
	if report != nil {
		if asJSON {
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			if writeErr := enc.Encode(report); writeErr != nil {
				return writeErr
			}
		} else {
			fmt.Fprintf(cmd.OutOrStdout(), "schema 2 → 3 ownership plan (%s), read-only\n", report.Policy)
			for _, f := range report.Findings {
				fmt.Fprintf(cmd.OutOrStdout(), "[%s] %s/%s %s: %v\n", f.Disposition, f.Collection, f.Key, f.Field, f.Evidence)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "blocked=%v; expected collection counts=%v\n", report.Blocked(), report.Counts)
		}
	}
	return err
}
