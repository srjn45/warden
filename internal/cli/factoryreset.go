package cli

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/srjn45/warden/internal/config"
	"github.com/srjn45/warden/internal/factoryreset"
)

func newFactoryResetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "factory-reset",
		Short: "Reset warden to a fresh-install state (scoped wipe of daemon data)",
		Long: `Reset warden's persisted state toward a clean new install.

This is destructive. Always stop the daemon before the offline wipe phase — the
session store lock is held while the hub is running.

Phases:
  1. Drain (when the daemon is reachable): terminate agents, cancel pipelines,
     stop autopilot runs, delete schedules.
  2. Offline wipe: remove on-disk stores for the selected scope.

Scopes:
  runtime  — live fleet + coordination scratch (active agents, pipelines,
             context, inbox, prompts/hints/settings/exits). Keeps archived
             history, projects, backends, metrics, and config.
  data     — every daemon store under data_dir (default). Keeps config.yaml,
             presets, prompt templates, and token unless you choose full.
  full     — data scope plus fresh config.yaml (unless --keep-config),
             presets, prompt templates, token, REPL history, and tutorial marker.

Examples:
  warden factory-reset --scope data --backup ~/.warden.bak --yes
  warden factory-reset --scope full --yes
  warden factory-reset --scope runtime --prune-worktrees --yes`,
		Args: cobra.NoArgs,
		RunE: runFactoryReset,
	}
	cmd.Flags().String("scope", string(factoryreset.ScopeData), "reset scope: runtime, data, or full")
	cmd.Flags().Bool("keep-config", false, "with --scope full, keep config.yaml instead of rewriting defaults")
	cmd.Flags().Bool("keep-backends", false, "keep the backend registry (tiers/models) for data/full scopes")
	cmd.Flags().String("backup", "", "copy data_dir here before wiping (must not exist)")
	cmd.Flags().Bool("prune-worktrees", false, "during drain, remove agent worktrees and prune orphans per repo")
	cmd.Flags().Bool("skip-drain", false, "skip the live drain phase (daemon may be down; wipe still requires it stopped)")
	cmd.Flags().Bool("yes", false, "confirm the destructive reset without prompting")
	return cmd
}

func runFactoryReset(cmd *cobra.Command, _ []string) error {
	cfgPath := configPathFor(cmd)
	cfg := config.Load(cfgPath)
	if a, _ := cmd.Flags().GetString("addr"); a != "" {
		cfg.Addr = a
	}

	scopeStr, _ := cmd.Flags().GetString("scope")
	scope := factoryreset.Scope(strings.TrimSpace(scopeStr))
	if scope != factoryreset.ScopeRuntime && scope != factoryreset.ScopeData && scope != factoryreset.ScopeFull {
		return fmt.Errorf("invalid --scope %q (want runtime, data, or full)", scopeStr)
	}

	yes, _ := cmd.Flags().GetBool("yes")
	if !yes {
		fmt.Fprintf(cmd.OutOrStdout(),
			"This will permanently reset warden scope=%s under %s.\nRe-run with --yes to confirm.\n",
			scope, cfg.DataDir)
		return nil
	}

	skipDrain, _ := cmd.Flags().GetBool("skip-drain")
	pruneWT, _ := cmd.Flags().GetBool("prune-worktrees")
	out := cmd.OutOrStdout()

	if !skipDrain && daemonReachable("http://"+cfg.Addr) {
		fmt.Fprintln(out, "draining live state via daemon…")
		cl := clientFor(cmd)
		if err := factoryreset.Drain(cmd.Context(), cl, pruneWT, out); err != nil {
			fmt.Fprintf(out, "warning: drain had errors (continuing): %v\n", err)
		}
		fmt.Fprintln(out, "drain complete — stop the daemon before the wipe continues.")
		fmt.Fprintln(out, "waiting for daemon to stop…")
		if err := waitForDaemonDown(cmd, cfg.Addr, 30*time.Second); err != nil {
			return err
		}
	} else if daemonReachable("http://" + cfg.Addr) {
		return errors.New("daemon is still running — stop it (Ctrl+C the hub process) before wiping, or re-run with --skip-drain after stopping it manually")
	}

	opts := factoryreset.Options{
		DataDir:      cfg.DataDir,
		ConfigPath:   cfgPath,
		Scope:        scope,
		KeepConfig:   mustBool(cmd, "keep-config"),
		KeepBackends: mustBool(cmd, "keep-backends"),
		BackupPath:   strings.TrimSpace(mustString(cmd, "backup")),
	}

	if opts.BackupPath != "" {
		fmt.Fprintf(out, "backing up %s → %s\n", cfg.DataDir, opts.BackupPath)
	}
	if err := factoryreset.Execute(opts); err != nil {
		return err
	}

	fmt.Fprintf(out, "factory reset complete (scope=%s).\n", scope)
	if scope == factoryreset.ScopeFull && !opts.KeepConfig {
		fmt.Fprintf(out, "config reset to defaults: %s\n", cfgPath)
	}
	fmt.Fprintln(out, "start the hub with: warden daemon")
	return nil
}

func daemonReachable(base string) bool {
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Get(strings.TrimRight(base, "/") + "/healthz")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func waitForDaemonDown(cmd *cobra.Command, addr string, timeout time.Duration) error {
	base := "http://" + addr
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !daemonReachable(base) {
			return nil
		}
		select {
		case <-cmd.Context().Done():
			return cmd.Context().Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("daemon still reachable at %s after %s — stop the hub and re-run with --skip-drain", addr, timeout)
}

func mustBool(cmd *cobra.Command, name string) bool {
	v, _ := cmd.Flags().GetBool(name)
	return v
}

func mustString(cmd *cobra.Command, name string) string {
	v, _ := cmd.Flags().GetString(name)
	return v
}
