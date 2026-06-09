package cli

import (
	"os"

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
