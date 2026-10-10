// Package updater implements the first-class `warden update` self-updater:
// resolve a GitHub release, verify checksums, atomically swap the installed
// binary, re-sign on macOS, run migrations, restart the user service, and
// roll back if the new daemon fails its health probe.
package updater

import (
	"context"
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

	// Context bounds the whole transaction (default Background).
	Context context.Context
	// ReadyTimeout is the overall readiness deadline (default DefaultReadyTimeout).
	ReadyTimeout time.Duration
	// Preflight runs before anything is downloaded or swapped; nil skips it.
	Preflight func(ctx context.Context) (PreflightResult, error)
	// TargetPreflight runs the target binary's preflight against the live data dir; nil skips it.
	TargetPreflight func(ctx context.Context, targetBin string) (PreflightResult, error)

	Service   ServiceController // default: systemd --user / launchd
	Prober    Prober            // default: HTTP GET HealthURL
	Installer BinaryInstaller   // default: atomic swap of InstallBin
	Clock     Clock             // default: wall clock

	Codesign func(bin string) error
	Migrate  func() error
	// TargetSchema reports the data schema version the freshly installed binary
	// writes (0 when it predates the schema ledger). Readiness then requires the
	// restarted daemon to report that schema_version as well as the target
	// version. nil checks the version only.
	TargetSchema func(ctx context.Context, bin string) (int, error)
	// CurrentSchema is the data schema version the running (pre-update) binary
	// writes; a rollback must bring the daemon back on it. Used only when
	// TargetSchema is set.
	CurrentSchema int
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

	ctx := opts.Context
	t := &txn{
		opts: opts, svc: opts.Service, probe: opts.Prober, inst: opts.Installer,
		clock: opts.Clock, target: rel.Version,
	}
	t.capture(ctx)
	if err := t.preflight(ctx); err != nil {
		return res, err
	}

	staged, err := downloadAndVerify(opts, rel)
	if err != nil {
		return res, err
	}
	defer os.RemoveAll(staged.Dir)

	if err := t.targetPreflight(ctx, staged.Binary); err != nil {
		return res, err
	}

	rolledBack, err := t.apply(ctx, staged.Binary)
	if err != nil {
		res.RolledBack = rolledBack
		res.Message = err.Error()
		return res, err
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
	if opts.Context == nil {
		opts.Context = context.Background()
	}
	if opts.ReadyTimeout <= 0 {
		opts.ReadyTimeout = DefaultReadyTimeout
	}
	if opts.Clock == nil {
		opts.Clock = realClock{}
	}
	if opts.Prober == nil {
		opts.Prober = httpProber{url: opts.HealthURL}
	}
	if opts.Service == nil {
		p := opts.Prober
		opts.Service = newSystemController(opts.GOOS, func(ctx context.Context) bool {
			h, err := p.Probe(ctx)
			return err == nil && h.Status == "ok"
		})
	}
	if opts.Installer == nil {
		opts.Installer = fsInstaller{bin: opts.InstallBin}
	}
	if opts.Codesign == nil && opts.GOOS == "darwin" {
		opts.Codesign = codesignBinary
	}
	return nil
}

func stripV(v string) string {
	return strings.TrimPrefix(strings.TrimSpace(v), "v")
}

func versionsEqual(a, b string) bool {
	return stripV(a) == stripV(b)
}
