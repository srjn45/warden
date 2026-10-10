package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/srjn45/warden/internal/config"
	"github.com/srjn45/warden/internal/migrate"
	"github.com/srjn45/warden/internal/schema"
)

type initOutput struct {
	Status        string `json:"status"`
	DataDir       string `json:"data_dir"`
	SchemaVersion int    `json:"schema_version"`
	BinaryVersion string `json:"binary_version"`
	Stamped       bool   `json:"stamped"`
	Migrated      bool   `json:"migrated"`
}

func newInitCmd() *cobra.Command {
	var (
		dataDir string
		jsonOut bool
	)
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize the Warden data directory and schema ledger",
		Long: `Initialize the Warden data directory (~/.warden) and schema ledger.

On a fresh install, builds the data directory by running the migration chain from schema 0
and records the initial schema ledger (schema.json). On an existing installation without a ledger,
infers and stamps the baseline ledger from existing sentinel files. If migrations are pending,
applies them to bring the data directory up to the binary's schema version.

Upgrade and fresh install share the same migration code path.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if dataDir == "" {
				cfg := config.Load(configPathFor(cmd))
				dataDir = cfg.DataDir
			}

			if err := os.MkdirAll(dataDir, 0o700); err != nil {
				return fmt.Errorf("create data dir %s: %w", dataDir, err)
			}

			l, stamped, err := schema.Ensure(dataDir, version)
			if err != nil {
				return fmt.Errorf("ensure schema ledger: %w", err)
			}

			migrated := false
			if l.SchemaVersion < schema.SchemaVersion {
				rn := migrate.NewRunner(nil)
				if err := rn.Apply(migrate.Env{
					DataDir:       dataDir,
					BinaryVersion: version,
					Context:       cmd.Context(),
				}, schema.SchemaVersion); err != nil {
					return fmt.Errorf("apply migrations: %w", err)
				}
				migrated = true
				if updatedLedger, loadErr := schema.Load(dataDir); loadErr == nil {
					l = updatedLedger
				}
			}

			// Ensure standard ScrivaDB store directories exist
			for _, storeRel := range migrate.KnownStores {
				_ = os.MkdirAll(filepath.Join(dataDir, filepath.FromSlash(storeRel)), 0o700)
			}

			out := initOutput{
				Status:        "ok",
				DataDir:       dataDir,
				SchemaVersion: l.SchemaVersion,
				BinaryVersion: l.BinaryVersion,
				Stamped:       stamped,
				Migrated:      migrated,
			}

			if jsonOut {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(out)
			}

			action := "ready"
			if stamped {
				action = "initialized"
			} else if migrated {
				action = "migrated"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "warden data directory %s: %s (schema v%d)\n", action, dataDir, l.SchemaVersion)
			return nil
		},
	}

	cmd.Flags().StringVar(&dataDir, "data-dir", "", "data directory (default from config: ~/.warden)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "print machine-readable JSON output")

	return cmd
}
