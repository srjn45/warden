package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/srjn45/warden/internal/config"
	"github.com/srjn45/warden/internal/updater"
)

// schema3Hooks wires the updater's format-3 seams to the real filesystem and
// the staged binary. They are consulted only when the staged binary reports
// data format 3, so a schema-2 update never reaches them.
func schema3Hooks(cfgPath, dataDir string) *updater.Schema3Hooks {
	return &updater.Schema3Hooks{
		ConfigPaths: func() ([]string, error) { return schema3ConfigPaths(cfgPath, dataDir) },
		ReconcileConfig: func() error {
			return config.Reconcile(cfgPath)
		},
		TargetInfo:      binaryTargetInfo,
		VerifyReadiness: execVerifyReadiness,
	}
}

// schema3ConfigPaths selects the config files the update may alter and a
// rollback must restore. The paired backup lives in the data dir, so a config
// file elsewhere cannot be restored by it and the update is refused rather than
// silently losing the rollback guarantee.
func schema3ConfigPaths(cfgPath, dataDir string) ([]string, error) {
	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(cfgPath)
	if err != nil {
		return nil, err
	}
	dir, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, err
	}
	if filepath.Dir(abs) != dir {
		return nil, fmt.Errorf("schema3: config %s is outside the data directory %s; move it into the data directory or pass --config for a file inside it so the paired rollback can restore it", abs, dir)
	}
	return []string{filepath.Base(abs)}, nil
}

func binaryTargetInfo(ctx context.Context, bin string) (updater.TargetInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "version", "--json").Output()
	if err != nil {
		return updater.TargetInfo{}, fmt.Errorf("%s version --json: %w", bin, err)
	}
	var bi buildInfo
	if err := json.Unmarshal(out, &bi); err != nil {
		return updater.TargetInfo{}, fmt.Errorf("decode %s version --json: %w", bin, err)
	}
	return updater.TargetInfo{Version: bi.Version, Schema: bi.SchemaVersion, Capabilities: bi.UpdaterCapabilities}, nil
}

// execVerifyReadiness asks the started target to prove it owns the exact
// transaction: `<bin> migrate --verify-ownership …`. A target that lacks the
// subcommand fails closed and the update rolls back.
func execVerifyReadiness(ctx context.Context, ev updater.ReadinessEvidence) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	bin := ev.TargetBinaryPath
	if bin == "" {
		return false, fmt.Errorf("target binary path unknown")
	}
	cmd := exec.CommandContext(ctx, bin, "migrate", "--verify-ownership", "--data-dir", ev.DataDir,
		"--transaction", ev.Transaction, "--target-sha", ev.TargetBinarySHA, "--manifest-sha", ev.ManifestSHA256)
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	if err := cmd.Run(); err != nil {
		return false, fmt.Errorf("%s migrate --verify-ownership: %w: %s", bin, err, bytes.TrimSpace(buf.Bytes()))
	}
	return true, nil
}
