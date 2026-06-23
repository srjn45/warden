package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"github.com/srjn45/warden/internal/auth"
)

// launchdLabel is the LaunchAgent label the macOS installer registers; it must
// match LABEL in scripts/common.sh.
const launchdLabel = "com.srajanpathak.warden"

// applyRotatedToken restarts the user-level warden service so the daemon picks
// up a freshly written bearer token, returning a short description of what it
// did. It is platform-aware because the two service managers source the token
// differently: the systemd unit reads it from an EnvironmentFile (so a plain
// restart re-reads the new value), while launchd has no EnvironmentFile and the
// installer inlines the token into the LaunchAgent plist — which therefore must
// be rewritten before the kickstart for the rotation to take effect.
func applyRotatedToken(token string) (string, error) {
	switch runtime.GOOS {
	case "linux":
		if _, err := exec.LookPath("systemctl"); err != nil {
			return "", fmt.Errorf("systemctl not found; restart the warden daemon manually to apply the new token")
		}
		if out, err := exec.Command("systemctl", "--user", "restart", "warden").CombinedOutput(); err != nil {
			return "", fmt.Errorf("systemctl --user restart warden: %v: %s", err, strings.TrimSpace(string(out)))
		}
		return "restarted systemd user service 'warden'", nil
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		plist := filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist")
		if err := rewritePlistToken(plist, token); err != nil {
			return "", err
		}
		uid := strconv.Itoa(os.Getuid())
		if out, err := exec.Command("launchctl", "kickstart", "-k", "gui/"+uid+"/"+launchdLabel).CombinedOutput(); err != nil {
			return "", fmt.Errorf("launchctl kickstart: %v: %s", err, strings.TrimSpace(string(out)))
		}
		return "restarted launchd service '" + launchdLabel + "'", nil
	default:
		return "", fmt.Errorf("automatic restart unsupported on %s; restart the warden daemon manually to apply the new token", runtime.GOOS)
	}
}

// plistTokenRe captures the inlined WARDEN_TOKEN value in a launchd plist so it
// can be replaced. Group 1 is the opening <key>…</key><string>, group 2 the
// closing </string>; the value (anything that is not a tag) sits between them.
var plistTokenRe = regexp.MustCompile(`(<key>` + auth.TokenEnv + `</key>\s*<string>)[^<]*(</string>)`)

// rewritePlistToken replaces the inlined WARDEN_TOKEN value in a launchd plist.
// macOS has no EnvironmentFile equivalent, so the installer embeds the token in
// the plist; rotation must update it there for a kickstart to serve the new
// secret. Existing file permissions are preserved (os.WriteFile leaves the mode
// of an existing file untouched), keeping the plist's secrecy intact.
func rewritePlistToken(path, token string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read launch agent %s: %w (is the remote service installed?)", path, err)
	}
	if !plistTokenRe.Match(b) {
		return fmt.Errorf("no %s entry found in %s; reinstall the service to enable auth before rotating", auth.TokenEnv, path)
	}
	updated := plistTokenRe.ReplaceAll(b, []byte("${1}"+token+"${2}"))
	return os.WriteFile(path, updated, 0o600)
}
