// Package updater implements the first-class `warden update` self-updater:
// resolve a GitHub release, verify checksums, atomically swap the installed
// binary, re-sign on macOS, run migrations, restart the user service, and
// roll back if the new daemon fails its health probe.
package updater

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	// DefaultRepo is the GitHub repository that publishes warden releases.
	DefaultRepo = "srjn45/warden"
	// CodesignIdentity matches scripts/common.sh CODESIGN_IDENTITY.
	CodesignIdentity = "warden-codesign"
	// LaunchdLabel matches scripts/common.sh LABEL.
	LaunchdLabel = "com.srajanpathak.warden"
	// DefaultHealthURL is the loopback health probe used after a service restart.
	DefaultHealthURL = "http://127.0.0.1:8765/healthz"
)

// Result is the outcome of Check or Apply.
type Result struct {
	CurrentVersion string
	TargetVersion  string // without leading "v"
	TargetTag      string // with leading "v"
	UpToDate       bool
	Updated        bool
	RolledBack     bool
	Message        string
}

// Options configures an update run. Zero values pick production defaults;
// tests inject HTTP, paths, and side-effect hooks.
type Options struct {
	CurrentVersion string
	TargetVersion  string // empty = latest; accepts "1.2.3" or "v1.2.3"
	Force          bool
	CheckOnly      bool

	Repo       string // default DefaultRepo
	GOOS       string
	GOARCH     string
	InstallBin string // default ~/.local/bin/warden
	StagingDir string // default ~/.warden/tmp
	HealthURL  string // default DefaultHealthURL
	AssetBase  string // override download base (tests); empty → GitHub releases

	HTTPClient *http.Client
	Stdout     io.Writer

	Restart     func() error
	Codesign    func(bin string) error
	Migrate     func() error
	HealthProbe func(url string) error
	Sleep       func(time.Duration)
}

// Check reports whether an update is available without applying it.
func Check(opts Options) (Result, error) {
	opts.CheckOnly = true
	return run(opts)
}

// Apply downloads, verifies, swaps, migrates, restarts, and health-checks.
func Apply(opts Options) (Result, error) {
	opts.CheckOnly = false
	return run(opts)
}

func run(opts Options) (Result, error) {
	if err := normalizeOptions(&opts); err != nil {
		return Result{}, err
	}
	out := opts.Stdout

	rel, err := resolveRelease(opts)
	if err != nil {
		return Result{}, err
	}

	res := Result{
		CurrentVersion: stripV(opts.CurrentVersion),
		TargetVersion:  rel.Version,
		TargetTag:      rel.Tag,
	}

	if !opts.Force && versionsEqual(opts.CurrentVersion, rel.Version) {
		res.UpToDate = true
		res.Message = fmt.Sprintf("already up to date (v%s)", res.CurrentVersion)
		fmt.Fprintln(out, res.Message)
		return res, nil
	}

	if opts.CheckOnly {
		res.Message = fmt.Sprintf("update available: v%s → v%s", res.CurrentVersion, res.TargetVersion)
		if versionsEqual(opts.CurrentVersion, rel.Version) && opts.Force {
			res.Message = fmt.Sprintf("reinstall available: v%s (--force)", res.TargetVersion)
		}
		fmt.Fprintln(out, res.Message)
		return res, nil
	}

	fmt.Fprintf(out, "updating warden v%s → v%s…\n", res.CurrentVersion, res.TargetVersion)

	staged, err := downloadAndVerify(opts, rel)
	if err != nil {
		return res, err
	}
	defer os.RemoveAll(staged.Dir)

	backup, err := swapBinary(opts.InstallBin, staged.Binary)
	if err != nil {
		return res, err
	}

	rollback := func(reason error) (Result, error) {
		res.RolledBack = true
		if backup != "" {
			if rbErr := restoreBackup(opts.InstallBin, backup); rbErr != nil {
				return res, fmt.Errorf("%w (also failed to restore backup: %v)", reason, rbErr)
			}
		}
		res.Message = fmt.Sprintf("update failed; rolled back to v%s: %v", res.CurrentVersion, reason)
		fmt.Fprintln(out, res.Message)
		return res, reason
	}

	if opts.Codesign != nil {
		if err := opts.Codesign(opts.InstallBin); err != nil {
			return rollback(fmt.Errorf("codesign: %w", err))
		}
	}

	if opts.Migrate != nil {
		fmt.Fprintln(out, "running migrations…")
		if err := opts.Migrate(); err != nil {
			return rollback(fmt.Errorf("migrate: %w", err))
		}
	}

	if opts.Restart != nil {
		fmt.Fprintln(out, "restarting service…")
		if err := opts.Restart(); err != nil {
			return rollback(fmt.Errorf("restart: %w", err))
		}
	}

	if opts.HealthProbe != nil && opts.HealthURL != "" {
		fmt.Fprintf(out, "probing %s…\n", opts.HealthURL)
		if err := waitHealthy(opts); err != nil {
			return rollback(fmt.Errorf("health check: %w", err))
		}
	}

	if backup != "" {
		_ = os.Remove(backup)
	}

	res.Updated = true
	res.Message = fmt.Sprintf("updated to v%s", res.TargetVersion)
	fmt.Fprintln(out, res.Message)
	return res, nil
}

func normalizeOptions(opts *Options) error {
	if strings.TrimSpace(opts.CurrentVersion) == "" {
		opts.CurrentVersion = "dev"
	}
	if opts.Repo == "" {
		opts.Repo = DefaultRepo
	}
	if opts.GOOS == "" {
		opts.GOOS = runtime.GOOS
	}
	if opts.GOARCH == "" {
		opts.GOARCH = runtime.GOARCH
	}
	if opts.GOOS != "linux" && opts.GOOS != "darwin" {
		return fmt.Errorf("unsupported OS %q — warden update supports linux and darwin only", opts.GOOS)
	}
	if opts.GOARCH != "amd64" && opts.GOARCH != "arm64" {
		return fmt.Errorf("unsupported architecture %q — warden update supports amd64 and arm64 only", opts.GOARCH)
	}
	if opts.InstallBin == "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return fmt.Errorf("resolve home directory: %w", err)
		}
		opts.InstallBin = filepath.Join(home, ".local", "bin", "warden")
	}
	if opts.StagingDir == "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return fmt.Errorf("resolve home directory: %w", err)
		}
		opts.StagingDir = filepath.Join(home, ".warden", "tmp")
	}
	if opts.HealthURL == "" {
		opts.HealthURL = DefaultHealthURL
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 60 * time.Second}
	}
	if opts.Stdout == nil {
		opts.Stdout = io.Discard
	}
	if opts.Sleep == nil {
		opts.Sleep = time.Sleep
	}
	if opts.Restart == nil {
		opts.Restart = restartService
	}
	if opts.Codesign == nil && opts.GOOS == "darwin" {
		opts.Codesign = codesignBinary
	}
	if opts.HealthProbe == nil {
		opts.HealthProbe = probeHealth
	}
	return nil
}

func stripV(v string) string {
	return strings.TrimPrefix(strings.TrimSpace(v), "v")
}

func versionsEqual(a, b string) bool {
	return stripV(a) == stripV(b)
}

func waitHealthy(opts Options) error {
	var last error
	for i := 0; i < 20; i++ {
		if err := opts.HealthProbe(opts.HealthURL); err == nil {
			return nil
		} else {
			last = err
		}
		opts.Sleep(500 * time.Millisecond)
	}
	if last == nil {
		last = fmt.Errorf("timed out waiting for %s", opts.HealthURL)
	}
	return last
}
