package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/srjn45/warden/internal/config"
	"github.com/srjn45/warden/internal/migrate"
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
		checkFlag   bool
		applyFlag   bool
		resumeFlag  bool
		restoreFlag bool
		jsonFlag    bool
		targetVer   int
		dataDirFlag string
	)

	cmd := &cobra.Command{
		Use:           "migrate",
		Short:         "Inspect, apply, resume, or restore data format migrations",
		SilenceUsage:  true,
		SilenceErrors: true,
		Long: `Run data format migrations for the Warden data directory.

Actions (select one):
  --check    Read-only preflight check across all stores and pending migrations
  --apply    Apply pending migrations up to the target schema version
  --resume   Resume an interrupted migration recorded in the ledger journal
  --restore  Restore from pre-migration snapshot after an interrupted migration`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Validate mutually exclusive flags
			count := 0
			if checkFlag {
				count++
			}
			if applyFlag {
				count++
			}
			if resumeFlag {
				count++
			}
			if restoreFlag {
				count++
			}

			if count > 1 {
				return errors.New("cannot specify more than one of --check, --apply, --resume, --restore")
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

			switch {
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
	cmd.Flags().BoolVar(&applyFlag, "apply", false, "apply pending migrations up to the target schema version")
	cmd.Flags().BoolVar(&resumeFlag, "resume", false, "resume an interrupted migration")
	cmd.Flags().BoolVar(&restoreFlag, "restore", false, "restore from pre-migration snapshot after an interrupted migration")
	cmd.Flags().BoolVar(&jsonFlag, "json", false, "output results in JSON format")
	cmd.Flags().IntVar(&targetVer, "target", schema.SchemaVersion, "target schema version to migrate to")
	cmd.Flags().StringVar(&dataDirFlag, "data-dir", "", "path to the Warden data directory")

	return cmd
}
