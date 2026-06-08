package cli

import (
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
	"github.com/srajanpathak/warden/internal/config"
	"github.com/srajanpathak/warden/internal/mcp"
)

func newMCPCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "mcp",
		Short: "Run the MCP stdio server so an orchestrator Claude can manage agents",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := config.Load()
			if a, _ := cmd.Flags().GetString("addr"); a != "" {
				cfg.Addr = a
			}
			srv := mcp.NewServer("http://" + cfg.Addr)
			return srv.Run(cmd.Context(), &mcpsdk.StdioTransport{})
		},
	}
}
