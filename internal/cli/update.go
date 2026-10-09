package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/srjn45/warden/internal/backendstore"
	"github.com/srjn45/warden/internal/config"
	"github.com/srjn45/warden/internal/schema"
	"github.com/srjn45/warden/internal/updater"
)

func newUpdateCmd() *cobra.Command {
	var (
		checkOnly bool
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
  --version <tag>    install a specific release (e.g. 9.9.0 or v9.9.0)
  --force            reinstall even when already on the target version
  --ready-timeout    overall deadline for the daemon to become healthy (default 90s)

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
				ReadyTimeout:   ready,
				Context:        cmd.Context(),
				Preflight: func(ctx context.Context) (updater.PreflightResult, error) {
					return backendPreflight(ctx, filepath.Join(cfg.DataDir, "backends"))
				},
				Stdout: cmd.OutOrStdout(),
				Migrate: func() error {
					return config.Reconcile(cfgPath)
				},
				TargetSchema:  binarySchemaVersion,
				CurrentSchema: schema.SchemaVersion,
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
			_ = res
			return updateRecoveryGuidance(err)
		},
	}
	cmd.Flags().BoolVar(&checkOnly, "check", false, "query and print whether an update is available without applying it")
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

// updateRecoveryGuidance appends the backend-registry recovery procedure
// (exact command + report location) to an update failure caused by the
// registry: the preflight blocker, or a new daemon that refused to start on it
// (recognised in the captured journal/stderr tail). Other errors are unchanged.
func updateRecoveryGuidance(err error) error {
	var pe *updater.PreflightError
	if errors.As(err, &pe) {
		return fmt.Errorf("%w\n%s", err, backendRecoverySteps("", ""))
	}
	return withBackendRecoverySteps(err)
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
	for _, c := range rep.Collections {
		switch c.Verdict {
		case backendstore.VerdictRecoverable:
			res.Notes = append(res.Notes, fmt.Sprintf("%s: %s", c.Name, strings.Join(c.Codes, ", ")))
		case backendstore.VerdictAmbiguous:
			why := strings.Join(c.Reasons, "; ")
			res.Blockers = append(res.Blockers, fmt.Sprintf("%s: %s (%s)", c.Name, strings.Join(c.Codes, ", "), why))
		}
	}
	return res, nil
}
