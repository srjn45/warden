package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/srjn45/warden/internal/config"
	"github.com/srjn45/warden/internal/updater"
)

func newRollbackCmd() *cobra.Command {
	var (
		yes    bool
		ready  time.Duration
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "rollback",
		Short: "Roll back the previous warden update",
		Long: `Roll back the most recent warden update.

Restores the previous binary (.bak). If no schema change happened it is a
plain binary swap. Otherwise it restores the pre-update snapshot and warns
that changes made since the update are lost, requiring confirmation (--yes
to skip confirmation).`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfgPath := configPathFor(cmd)
			cfg := config.Load(cfgPath)
			healthURL := "http://" + strings.TrimSpace(cfg.Addr) + "/healthz"
			if strings.TrimSpace(cfg.Addr) == "" {
				healthURL = updater.DefaultHealthURL
			}

			opts := updater.RollbackOptions{
				DataDir:      cfg.DataDir,
				HealthURL:    healthURL,
				ReadyTimeout: ready,
				Force:        yes,
				Stdout:       cmd.OutOrStdout(),
				Stderr:       cmd.ErrOrStderr(),
			}
			if home, err := os.UserHomeDir(); err == nil && home != "" {
				opts.InstallBin = filepath.Join(home, ".local", "bin", "warden")
			}

			res, err := updater.Rollback(opts)
			if err != nil {
				return err
			}
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(res)
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip loss-of-changes confirmation prompt")
	cmd.Flags().DurationVar(&ready, "ready-timeout", 90*time.Second, "overall deadline for the restored daemon to report healthy")
	cmd.Flags().BoolVar(&asJSON, "json", false, "output result as JSON")
	return cmd
}
