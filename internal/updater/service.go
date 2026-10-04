package updater

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

func restartService() error {
	switch runtime.GOOS {
	case "linux":
		if _, err := exec.LookPath("systemctl"); err != nil {
			return fmt.Errorf("systemctl not found; restart the warden daemon manually")
		}
		out, err := exec.Command("systemctl", "--user", "restart", "warden").CombinedOutput()
		if err != nil {
			return fmt.Errorf("systemctl --user restart warden: %v: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	case "darwin":
		if _, err := exec.LookPath("launchctl"); err != nil {
			return fmt.Errorf("launchctl not found; restart the warden daemon manually")
		}
		uid := strconv.Itoa(os.Getuid())
		target := "gui/" + uid + "/" + LaunchdLabel
		out, err := exec.Command("launchctl", "kickstart", "-k", target).CombinedOutput()
		if err != nil {
			return fmt.Errorf("launchctl kickstart %s: %v: %s", target, err, strings.TrimSpace(string(out)))
		}
		return nil
	default:
		return fmt.Errorf("automatic restart unsupported on %s", runtime.GOOS)
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
	if out, err := exec.Command("security", "find-certificate", "-c", CodesignIdentity).CombinedOutput(); err != nil {
		_ = out
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

func probeHealth(url string) error {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s returned %d", url, resp.StatusCode)
	}
	return nil
}
