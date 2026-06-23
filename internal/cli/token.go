package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/srjn45/warden/internal/auth"
)

func newTokenCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Manage the daemon's remote-access bearer token",
	}
	cmd.AddCommand(newTokenGenerateCmd())
	return cmd
}

func newTokenGenerateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "generate",
		Short: "Generate a random bearer token for remote (non-loopback) access",
		Long: "Generate a cryptographically random 256-bit bearer token and print it to stdout.\n\n" +
			"warden does not store the token. Export it so the daemon picks it up:\n\n" +
			"  export " + auth.TokenEnv + "=$(warden token generate)\n\n" +
			"The token is required before the daemon will bind to a non-loopback address.\n" +
			"Treat it like a password.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			tok, err := auth.GenerateToken()
			if err != nil {
				return fmt.Errorf("generate token: %w", err)
			}
			fmt.Fprintln(cmd.OutOrStdout(), tok)
			return nil
		},
	}
}
