package cli

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/srjn45/warden/internal/config"
	"github.com/srjn45/warden/internal/migrate"
)

// newInspectLegacyCmd is the retained read-only inspector for schema-1 stores
// kept on disk after the schema-2 cutover. It has no write flags by design.
func newInspectLegacyCmd() *cobra.Command {
	var (
		jsonFlag    bool
		dataDirFlag string
	)
	cmd := &cobra.Command{
		Use:           "legacy",
		Short:         "Read-only inspection of retained schema-1 stores vs warden-db",
		SilenceUsage:  true,
		SilenceErrors: true,
		Long: `Inspect the legacy schema-1 stores kept on disk after the schema-2 cutover:
legacy source paths, per-collection record counts compared with the canonical
warden-db, and the cutover backup's verification evidence.

Strictly read-only and never an authority: legacy stores are opened only
through private copies, nothing is written to the data dir (a before/after
fingerprint must match), and a data dir that has not cut over to schema 2 is
refused because there the legacy stores are the live authority. Canonical-only
records are expected after runtime use. Legacy data is never deleted.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			dataDir := dataDirFlag
			if dataDir == "" {
				dataDir = config.Load(configPathFor(cmd)).DataDir
			}
			rep, err := migrate.InspectLegacy(cmd.Context(), dataDir)
			if rep != nil && jsonFlag {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				if jerr := enc.Encode(rep); jerr != nil {
					return jerr
				}
			} else if rep != nil {
				fmt.Fprint(cmd.OutOrStdout(), rep.Summary())
			}
			if err != nil {
				if errors.Is(err, migrate.ErrLegacyLiveAuthority) {
					return err
				}
				return fmt.Errorf("inspect legacy: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonFlag, "json", false, "output results in JSON format")
	cmd.Flags().StringVar(&dataDirFlag, "data-dir", "", "path to the Warden data directory")
	return cmd
}
