package cli

import (
	"errors"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
	"github.com/srjn45/warden/internal/client"
	"github.com/srjn45/warden/internal/config"
)

func clientFor(cmd *cobra.Command) *client.Client {
	cfg := config.Load()
	if a, _ := cmd.Flags().GetString("addr"); a != "" {
		cfg.Addr = a
	}
	return client.New("http://" + cfg.Addr)
}

// envID reads an agent identity env var, preferring the canonical WARDEN_<name>
// and falling back to the legacy AGENTCTL_<name> for back-compat. Used for
// SESSION_ID, PIPELINE_ID, and JOB_ID, which the lifecycle injects into each
// agent's tmux session under both prefixes.
func envID(name string) string {
	if v := os.Getenv("WARDEN_" + name); v != "" {
		return v
	}
	return os.Getenv("AGENTCTL_" + name)
}

// isCommandNotFound checks if an error indicates a command was not found.
// Returns true for exec.ErrNotFound or errors containing "executable file not found".
func isCommandNotFound(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, exec.ErrNotFound) {
		return true
	}
	return strings.Contains(err.Error(), "executable file not found")
}

// installHint returns a platform-specific installation hint for a command.
func installHint(cmd string) string {
	switch cmd {
	case "tmux":
		return "Install: brew install tmux (macOS) or apt install tmux (Linux)"
	case "gh":
		return "Install: brew install gh (macOS) or apt install gh (Linux)\nOr visit: https://cli.github.com"
	case "claude":
		return "Install Claude Code from https://claude.ai/download"
	default:
		return "Install " + cmd + " to continue"
	}
}
