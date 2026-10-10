//go:build unix

package lifecycle

import (
	"os/exec"
	"syscall"
)

// ownProcessGroup starts cmd in its own process group and makes context
// cancellation kill the whole group, so a headless AI CLI's helper children
// (node workers, provider clients) die with it instead of outliving the call.
func ownProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Negative pid targets the group; fall back to the leader alone.
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
}
