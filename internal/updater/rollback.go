package updater

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/schema"
)

// RollbackOptions configures a rollback operation.
type RollbackOptions struct {
	DataDir      string
	InstallBin   string
	HealthURL    string
	ReadyTimeout time.Duration
	Force        bool // skip confirmation prompt
	Service      ServiceController
	Prober       Prober
	Installer    BinaryInstaller
	Clock        Clock
	Stdout       io.Writer
	Stderr       io.Writer
	Confirm      func(prompt string) bool
}

// RollbackResult is the outcome of a Rollback call.
type RollbackResult struct {
	PriorVersion  string `json:"prior_version,omitempty"`
	TargetVersion string `json:"target_version,omitempty"`
	SchemaChanged bool   `json:"schema_changed"`
	DataRestored  bool   `json:"data_restored"`
	Message       string `json:"message"`
}

// Rollback restores the previous binary and, if schema changed, data from the latest snapshot.
func Rollback(opts RollbackOptions) (RollbackResult, error) {
	if opts.Stdout == nil {
		opts.Stdout = io.Discard
	}
	if opts.Stderr == nil {
		opts.Stderr = io.Discard
	}
	if opts.Clock == nil {
		opts.Clock = realClock{}
	}
	if opts.ReadyTimeout <= 0 {
		opts.ReadyTimeout = DefaultReadyTimeout
	}
	if opts.DataDir == "" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			opts.DataDir = filepath.Join(home, ".warden")
		}
	}
	if opts.InstallBin == "" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			opts.InstallBin = filepath.Join(home, ".local", "bin", "warden")
		}
	}
	if opts.HealthURL == "" {
		opts.HealthURL = DefaultHealthURL
	}
	if opts.Prober == nil {
		opts.Prober = httpProber{url: opts.HealthURL}
	}
	if opts.Service == nil {
		p := opts.Prober
		opts.Service = newSystemController("", func(ctx context.Context) bool {
			h, err := p.Probe(ctx)
			return err == nil && h.Status == "ok"
		})
	}
	if opts.Installer == nil {
		opts.Installer = fsInstaller{bin: opts.InstallBin}
	}

	ctx := context.Background()

	// 1. Find latest snapshot
	snapDir, snapErr := LatestSnapshot(opts.DataDir)
	hasSnapshot := snapErr == nil && snapDir != ""

	// 2. Locate backup binary
	bakPath := opts.InstallBin + ".bak"
	hasBak := false
	if fi, err := os.Stat(bakPath); err == nil && fi.Mode().IsRegular() {
		hasBak = true
	} else if hasSnapshot {
		cand1 := filepath.Join(snapDir, "bin", filepath.Base(opts.InstallBin))
		cand2 := filepath.Join(snapDir, "warden")
		if fi, err := os.Stat(cand1); err == nil && fi.Mode().IsRegular() {
			hasBak = true
			bakPath = cand1
		} else if fi, err := os.Stat(cand2); err == nil && fi.Mode().IsRegular() {
			hasBak = true
			bakPath = cand2
		}
	}

	if !hasBak && !hasSnapshot {
		return RollbackResult{}, fmt.Errorf("no backup binary (.bak) or data snapshot found to roll back to")
	}

	// 3. Determine if schema change occurred
	currLedger, _ := schema.Load(opts.DataDir)
	var snapLedger *schema.Ledger
	schemaChanged := false
	if hasSnapshot {
		snapLedger, _ = schema.Load(snapDir)
		if currLedger != nil && snapLedger != nil && currLedger.SchemaVersion != snapLedger.SchemaVersion {
			schemaChanged = true
		}
	}

	// 4. Prompt for confirmation if schema changed
	if schemaChanged {
		promptMsg := fmt.Sprintf("Rolling back will restore data to %s (schema v%d -> v%d).\nAny changes made since the update will be lost. Continue?",
			filepath.Base(snapDir), currLedger.SchemaVersion, snapLedger.SchemaVersion)
		if !opts.Force {
			if opts.Confirm != nil {
				if !opts.Confirm(promptMsg) {
					return RollbackResult{}, fmt.Errorf("rollback cancelled")
				}
			} else {
				fmt.Fprintf(opts.Stderr, "Warning: %s [y/N]: ", promptMsg)
				reader := bufio.NewReader(os.Stdin)
				line, _ := reader.ReadString('\n')
				ans := strings.ToLower(strings.TrimSpace(line))
				if ans != "y" && ans != "yes" {
					return RollbackResult{}, fmt.Errorf("rollback cancelled")
				}
			}
		}
	}

	// 5. Execute rollback
	// Check if daemon was running
	daemonRunning := false
	if h, err := opts.Prober.Probe(ctx); err == nil && h.Status == "ok" {
		daemonRunning = true
	}

	if daemonRunning {
		fmt.Fprintln(opts.Stdout, "stopping daemon…")
		_ = opts.Service.Stop(ctx)
	}

	// Restore data if schema changed
	dataRestored := false
	if schemaChanged && hasSnapshot {
		fmt.Fprintf(opts.Stdout, "restoring data snapshot from %s…\n", filepath.Base(snapDir))
		if err := RestoreSnapshot(snapDir, opts.DataDir); err != nil {
			return RollbackResult{}, fmt.Errorf("restore data snapshot: %w", err)
		}
		dataRestored = true
	}

	// Restore binary
	if hasBak {
		fmt.Fprintln(opts.Stdout, "restoring previous binary…")
		if err := opts.Installer.Restore(bakPath); err != nil {
			return RollbackResult{}, fmt.Errorf("restore binary: %w", err)
		}
	}

	// Restart service
	if daemonRunning {
		fmt.Fprintln(opts.Stdout, "restarting service…")
		if err := opts.Service.Restart(ctx); err != nil && !errors.Is(err, ErrNoServiceManager) {
			return RollbackResult{}, fmt.Errorf("restart service: %w", err)
		}

		// Wait ready
		wantVer := ""
		wantSchema := schemaAny
		if snapLedger != nil {
			wantVer = stripV(snapLedger.BinaryVersion)
			wantSchema = snapLedger.SchemaVersion
		}

		deadline := opts.Clock.Now().Add(opts.ReadyTimeout)
		delay := 250 * time.Millisecond
		var lastErr error
		for {
			if opts.Service.Exited(ctx) {
				return RollbackResult{}, fmt.Errorf("daemon exited during rollback startup: %s", opts.Service.Diagnostics(ctx))
			}
			h, err := opts.Prober.Probe(ctx)
			switch {
			case err != nil:
				lastErr = err
			case h.Status != "ok":
				lastErr = fmt.Errorf("health status %q", h.Status)
			case wantVer != "" && stripV(h.Version) != wantVer:
				lastErr = fmt.Errorf("version mismatch: got %s want %s", h.Version, wantVer)
			case wantSchema != schemaAny && h.SchemaVersion != wantSchema:
				lastErr = fmt.Errorf("schema mismatch: got %d want %d", h.SchemaVersion, wantSchema)
			default:
				lastErr = nil
			}
			if lastErr == nil {
				break
			}
			if !opts.Clock.Now().Before(deadline) || ctx.Err() != nil {
				return RollbackResult{}, fmt.Errorf("daemon not healthy after rollback within %s: %v", opts.ReadyTimeout, lastErr)
			}
			opts.Clock.Sleep(ctx, delay)
			if delay = delay * 3 / 2; delay > 2*time.Second {
				delay = 2 * time.Second
			}
		}
	}

	targetVer := ""
	if snapLedger != nil {
		targetVer = snapLedger.BinaryVersion
	}
	msg := "rollback complete"
	if schemaChanged {
		msg = fmt.Sprintf("rollback complete: restored binary and data to schema v%d", snapLedger.SchemaVersion)
	} else {
		msg = "rollback complete: binary swapped (no schema change)"
	}
	fmt.Fprintln(opts.Stdout, msg)

	return RollbackResult{
		TargetVersion: targetVer,
		SchemaChanged: schemaChanged,
		DataRestored:  dataRestored,
		Message:       msg,
	}, nil
}
