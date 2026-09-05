package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/srjn45/warden/internal/relay"
)

func newLoginCmd() *cobra.Command {
	var hubURL string
	var hostname string

	cmd := &cobra.Command{
		Use:   "login",
		Short: "Authenticate this node with a warden-hub relay using the device flow",
		Long: `Authenticate this node with a warden-hub relay using the device flow.

The node generates an ECDSA keypair locally and initiates device authorization with
the hub. You will receive a verification URL and user code to approve in your browser.
Once approved, the signed node certificate and CA chain are stored locally in
~/.warden/identity. The private key never leaves this machine.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if hostname == "" {
				h, err := os.Hostname()
				if err == nil {
					hostname = h
				}
			}

			opts := relay.LoginOptions{
				HubURL:   hubURL,
				Hostname: hostname,
				Out:      cmd.OutOrStdout(),
			}

			_, err := relay.Login(context.Background(), opts)
			if err != nil {
				return fmt.Errorf("login failed: %w", err)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&hubURL, "hub", "", fmt.Sprintf("warden-hub base URL (default %s)", relay.DefaultHubURL))
	cmd.Flags().StringVar(&hostname, "hostname", "", "display hostname override for this node")

	return cmd
}
