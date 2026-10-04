package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/srjn45/warden/internal/config"
	"github.com/srjn45/warden/internal/updater"
)

func newUpdateCmd() *cobra.Command {
	var (
		checkOnly bool
		force     bool
		pin       string
	)
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update the installed warden binary from GitHub Releases",
		Long: `Download a verified GitHub release archive, atomically replace
~/.local/bin/warden, re-sign on macOS when the warden-codesign identity is
present, run config migrations, restart the user-level daemon service, and
probe /healthz — rolling the binary back if the new daemon is unhealthy.

Flags:
  --check            report whether an update is available without applying it
  --version <tag>    install a specific release (e.g. 9.9.0 or v9.9.0)
  --force            reinstall even when already on the target version

Examples:
  warden update
  warden update --check
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

			opts := updater.Options{
				CurrentVersion: version,
				TargetVersion:  pin,
				Force:          force,
				CheckOnly:      checkOnly,
				HealthURL:      healthURL,
				Stdout:         cmd.OutOrStdout(),
				Migrate: func() error {
					return config.Reconcile(cfgPath)
				},
			}
			if home, err := os.UserHomeDir(); err == nil && home != "" {
				opts.StagingDir = filepath.Join(home, ".warden", "tmp")
				opts.InstallBin = filepath.Join(home, ".local", "bin", "warden")
			}

			var (
				res updater.Result
				err error
			)
			if checkOnly {
				res, err = updater.Check(opts)
			} else {
				res, err = updater.Apply(opts)
			}
			if err != nil {
				if res.RolledBack {
					return fmt.Errorf("%s", res.Message)
				}
				return err
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&checkOnly, "check", false, "query and print whether an update is available without applying it")
	cmd.Flags().BoolVar(&force, "force", false, "reinstall even when already on the target version")
	cmd.Flags().StringVar(&pin, "version", "", "install a specific release tag (e.g. 9.9.0 or v9.9.0)")
	return cmd
}
