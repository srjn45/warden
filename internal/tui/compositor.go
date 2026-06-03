package tui

import (
	"fmt"
	"os/exec"
	"path/filepath"
)

// chooseClassic decides whether to use the legacy single-pane TUI instead of the
// tmux-composited cockpit. We require a real, non-nested tmux: composited mode
// builds a new session and attaches to it, which can't be done cleanly from
// inside an existing tmux client.
func chooseClassic(classicFlag, tmuxAvailable, insideTmux bool) bool {
	return classicFlag || !tmuxAvailable || insideTmux
}

// tmuxAvailable reports whether the tmux binary is on PATH.
func tmuxAvailable() bool {
	_, err := exec.LookPath("tmux")
	return err == nil
}

func cockpitSession(pid int) string {
	return fmt.Sprintf("agentctl-tui-%d", pid)
}

func cockpitStateDir(base string, pid int) string {
	return filepath.Join(base, fmt.Sprintf("tui-%d", pid))
}
