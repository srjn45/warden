package cli

import (
	"github.com/spf13/cobra"
	"github.com/srajanpathak/agentctl/internal/client"
	"github.com/srajanpathak/agentctl/internal/config"
)

func clientFor(cmd *cobra.Command) *client.Client {
	cfg := config.Load()
	if a, _ := cmd.Flags().GetString("addr"); a != "" {
		cfg.Addr = a
	}
	return client.New("http://" + cfg.Addr)
}
