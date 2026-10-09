package updater

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	systemdUnit     = "warden"
	launchdErrorLog = "/tmp/warden.daemon.err"
	diagLines       = 40
)

// systemController drives the real systemd --user unit / launchd agent.
type systemController struct {
	goos string
	// healthy is consulted by State to tell a manual daemon from none.
	healthy func(ctx context.Context) bool
}

func newSystemController(goos string, healthy func(context.Context) bool) *systemController {
	return &systemController{goos: goos, healthy: healthy}
}

func sh(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func launchdTarget() string { return "gui/" + strconv.Itoa(os.Getuid()) + "/" + LaunchdLabel }

func (c *systemController) State(ctx context.Context) ServiceState {
	switch c.goos {
	case "linux":
		if _, err := exec.LookPath("systemctl"); err == nil {
			out, err := sh(ctx, "systemctl", "--user", "show", systemdUnit, "-p", "LoadState", "-p", "ActiveState")
			if err == nil && strings.Contains(out, "LoadState=loaded") {
				return ServiceState{Kind: ServiceSystemd, Active: strings.Contains(out, "ActiveState=active"), Detail: strings.ReplaceAll(out, "\n", " ")}
			}
		}
	case "darwin":
		if _, err := exec.LookPath("launchctl"); err == nil {
			out, err := sh(ctx, "launchctl", "print", launchdTarget())
			if err == nil {
				return ServiceState{Kind: ServiceLaunchd, Active: strings.Contains(out, "state = running"), Detail: launchdTarget()}
			}
		}
	}
	if c.healthy != nil && c.healthy(ctx) {
		return ServiceState{Kind: ServiceManual, Active: true, Detail: "daemon answers /healthz without a service manager"}
	}
	return ServiceState{Kind: ServiceNone}
}

func (c *systemController) Restart(ctx context.Context) error {
	st := c.State(ctx)
	switch st.Kind {
	case ServiceSystemd:
		if out, err := sh(ctx, "systemctl", "--user", "restart", systemdUnit); err != nil {
			return fmt.Errorf("systemctl --user restart %s: %v: %s", systemdUnit, err, out)
		}
		return nil
	case ServiceLaunchd:
		if out, err := sh(ctx, "launchctl", "kickstart", "-k", launchdTarget()); err != nil {
			return fmt.Errorf("launchctl kickstart %s: %v: %s", launchdTarget(), err, out)
		}
		return nil
	}
	return ErrNoServiceManager
}

func (c *systemController) Exited(ctx context.Context) bool {
	switch c.goos {
	case "linux":
		out, err := sh(ctx, "systemctl", "--user", "show", systemdUnit, "-p", "ActiveState", "-p", "SubState")
		if err != nil {
			return false
		}
		return strings.Contains(out, "ActiveState=failed") || strings.Contains(out, "ActiveState=inactive") ||
			strings.Contains(out, "SubState=auto-restart")
	case "darwin":
		out, err := sh(ctx, "launchctl", "print", launchdTarget())
		if err != nil {
			return false
		}
		return !strings.Contains(out, "state = running") && strings.Contains(out, "last exit code") &&
			!strings.Contains(out, "last exit code = 0")
	}
	return false
}

func (c *systemController) Diagnostics(ctx context.Context) string {
	switch c.goos {
	case "linux":
		out, _ := sh(ctx, "journalctl", "--user", "-u", systemdUnit, "-n", strconv.Itoa(diagLines), "--no-pager", "--output=cat")
		return out
	case "darwin":
		out, _ := sh(ctx, "tail", "-n", strconv.Itoa(diagLines), launchdErrorLog)
		return out
	}
	return ""
}

// httpProber decodes GET /healthz.
type httpProber struct {
	url    string
	client *http.Client
}

func (p httpProber) Probe(ctx context.Context) (Health, error) {
	client := p.client
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.url, nil)
	if err != nil {
		return Health{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return Health{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Health{}, fmt.Errorf("%s returned %d", p.url, resp.StatusCode)
	}
	var body struct {
		Status  string `json:"status"`
		Version string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return Health{}, fmt.Errorf("decode %s: %w", p.url, err)
	}
	return Health{Status: body.Status, Version: body.Version}, nil
}

// fsInstaller swaps the binary on disk.
type fsInstaller struct{ bin string }

func (f fsInstaller) Swap(newBin string) (string, error) { return swapBinary(f.bin, newBin) }
func (f fsInstaller) Restore(backup string) error        { return restoreBackup(f.bin, backup) }
func (f fsInstaller) Discard(backup string) {
	if backup != "" {
		_ = os.Remove(backup)
	}
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }
func (realClock) Sleep(ctx context.Context, d time.Duration) {
	select {
	case <-time.After(d):
	case <-ctx.Done():
	}
}

func codesignBinary(bin string) error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	if _, err := exec.LookPath("codesign"); err != nil {
		return nil // non-macOS toolchains / CI cross-builds
	}
	// Match install.sh: skip when the identity is absent (end-user machines
	// often lack the self-signed cert); only fail when signing is attempted
	// and fails.
	if _, err := exec.Command("security", "find-certificate", "-c", CodesignIdentity).CombinedOutput(); err != nil {
		return nil
	}
	out, err := exec.Command("codesign", "--force", "--sign", CodesignIdentity, "--identifier", LaunchdLabel, bin).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command("codesign", "--verify", "--strict", bin).CombinedOutput(); err != nil {
		return fmt.Errorf("codesign verify: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
