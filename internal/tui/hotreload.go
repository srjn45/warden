package tui

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/srjn45/warden/internal/updater"
)

// updateCheckInterval is how often the cockpit asks GitHub whether a newer
// release is available. Daemon version is polled on the ordinary 1s tick.
const updateCheckInterval = 5 * time.Minute

// localVersion is the TUI process's build version (set from cli via SetLocalVersion).
var localVersion = "dev"

// SetLocalVersion records the running binary's version for update/reload chips.
func SetLocalVersion(v string) {
	if strings.TrimSpace(v) != "" {
		localVersion = v
	}
}

// Hooks injectable in tests. Production defaults hit the updater package and
// syscall.Exec the current binary after Bubble Tea restores the terminal.
var (
	checkUpdateFn = defaultCheckUpdate
	applyUpdateFn = defaultApplyUpdate
	execSelfFn    = defaultExecSelf
)

type updateState struct {
	LocalVersion     string
	DaemonVersion    string
	AvailableVersion string // empty when up to date / unknown
}

func (s updateState) reloadPending() bool {
	return s.DaemonVersion != "" && !versionEqual(s.LocalVersion, s.DaemonVersion)
}

func (s updateState) updateAvailable() bool {
	return s.AvailableVersion != "" && !versionEqual(s.LocalVersion, s.AvailableVersion)
}

// updateChipText returns the footer chip for a pending reload or available update.
// Reload takes precedence over a GitHub update notice.
func updateChipText(s updateState) string {
	if s.reloadPending() {
		return fmt.Sprintf("[r] Warden upgraded to v%s — press 'r' to reload TUI", stripVer(s.DaemonVersion))
	}
	if s.updateAvailable() {
		return fmt.Sprintf("[u] Update to v%s available (press 'u')", stripVer(s.AvailableVersion))
	}
	return ""
}

type hotReloadKeyAction int

const (
	hotReloadKeyNone hotReloadKeyAction = iota
	hotReloadKeyPromptUpdate
	hotReloadKeyReload
)

// routeHotReloadKey decides whether a normal-mode key should start an update
// confirm or an in-place TUI reload. Returns hotReloadKeyNone when the key is
// not claimed (callers keep their existing bindings).
func routeHotReloadKey(key string, s updateState) hotReloadKeyAction {
	switch key {
	case "u":
		if s.updateAvailable() && !s.reloadPending() {
			return hotReloadKeyPromptUpdate
		}
	case "r":
		if s.reloadPending() {
			return hotReloadKeyReload
		}
	}
	return hotReloadKeyNone
}

func stripVer(v string) string {
	return strings.TrimPrefix(strings.TrimSpace(v), "v")
}

func versionEqual(a, b string) bool {
	return stripVer(a) == stripVer(b)
}

type healthMsg struct {
	version string
	err     error
}

type updateCheckMsg struct {
	available string // target version without requiring "v"; empty if up to date
	err       error
}

type updateApplyMsg struct {
	target  string
	err     error
	message string
}

func healthCmd(a api) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := bg()
		defer cancel()
		h, err := a.Health(ctx)
		if err != nil {
			return healthMsg{err: err}
		}
		return healthMsg{version: h.Version}
	}
}

func updateCheckCmd(current string) tea.Cmd {
	return func() tea.Msg {
		res, err := checkUpdateFn(current)
		if err != nil {
			return updateCheckMsg{err: err}
		}
		if res.UpToDate || res.TargetVersion == "" {
			return updateCheckMsg{}
		}
		return updateCheckMsg{available: res.TargetVersion}
	}
}

func updateApplyCmd(current string) tea.Cmd {
	return func() tea.Msg {
		res, err := applyUpdateFn(current)
		return updateApplyMsg{target: res.TargetVersion, err: err, message: res.Message}
	}
}

// reloadQuitCmd exits the Bubble Tea program without tearing down the cockpit
// so RunControlPane can syscall.Exec the (possibly new) binary in place.
func reloadQuitCmd() tea.Cmd {
	return tea.Quit
}

func defaultCheckUpdate(current string) (updater.Result, error) {
	return updater.Check(updater.Options{
		CurrentVersion: current,
		Stdout:         io.Discard,
	})
}

func defaultApplyUpdate(current string) (updater.Result, error) {
	return updater.Apply(updater.Options{
		CurrentVersion: current,
		Stdout:         io.Discard,
	})
}

func defaultExecSelf() error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}
	return syscall.Exec(self, os.Args, os.Environ())
}
