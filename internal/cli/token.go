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
	cmd.AddCommand(newTokenShowCmd())
	cmd.AddCommand(newTokenRotateCmd())
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
			"Treat it like a password.\n\n" +
			"To mint a read-only token, generate one and export it as " + auth.ReadonlyTokenEnv + ":\n\n" +
			"  export " + auth.ReadonlyTokenEnv + "=$(warden token generate)\n\n" +
			"A read-only token may read everything (all GETs plus the live event stream) but\n" +
			"is denied every state-changing action and the interactive attach. It only works\n" +
			"alongside a primary " + auth.TokenEnv + "; the daemon refuses to start with a read-only\n" +
			"token but no primary token. (The token value is identical either way — what makes\n" +
			"it read-only is the env var you assign it to.)",
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

func newTokenShowCmd() *cobra.Command {
	var readonly bool
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Print the current bearer token (for pasting into a remote client)",
		Long: "Print the bearer token local clients resolve: " + auth.TokenEnv + " if exported,\n" +
			"otherwise the token a managed install persists in " + auth.DefaultTokenFile() + ".\n\n" +
			"Use this to retrieve the secret to paste into the mobile web dashboard. The token\n" +
			"is printed to stdout (so it pipes cleanly); its source is noted on stderr.\n\n" +
			"With --readonly, print the read-only token instead (" + auth.ReadonlyTokenEnv + " if\n" +
			"exported, otherwise its line in the token file).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			envName, fromEnv, resolve := auth.TokenEnv, auth.TokenFromEnv, auth.ResolveToken
			if readonly {
				envName, fromEnv, resolve = auth.ReadonlyTokenEnv, auth.ReadonlyTokenFromEnv, auth.ResolveReadonlyToken
			}
			tok := resolve()
			if tok == "" {
				if readonly {
					return fmt.Errorf("no read-only token configured: export %s (run `warden token generate`)", auth.ReadonlyTokenEnv)
				}
				return fmt.Errorf("no bearer token configured: export %s or run `warden token rotate` (remote installs provision one automatically)", auth.TokenEnv)
			}
			fmt.Fprintln(cmd.OutOrStdout(), tok)
			source := "$" + envName
			if fromEnv() == "" {
				source = auth.DefaultTokenFile()
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "source: %s\n", source)
			return nil
		},
	}
	cmd.Flags().BoolVar(&readonly, "readonly", false, "print the read-only token ("+auth.ReadonlyTokenEnv+") instead of the primary")
	return cmd
}

func newTokenRotateCmd() *cobra.Command {
	var noRestart bool
	cmd := &cobra.Command{
		Use:   "rotate",
		Short: "Generate a new bearer token, persist it, and restart the daemon",
		Long: "Rotate the daemon's bearer token: generate a fresh 256-bit secret, write it to\n" +
			auth.DefaultTokenFile() + " (chmod 600), and restart the managed warden service so\n" +
			"the new token takes effect immediately. The new token is printed to stdout.\n\n" +
			"After rotating, update remote clients (paste the new token into the mobile\n" +
			"dashboard) and re-export " + auth.TokenEnv + " in any shell that held the old value.\n\n" +
			"Use --no-restart to write the new token without restarting; you must then restart\n" +
			"the daemon yourself before the new token is honored.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			path := auth.DefaultTokenFile()
			if path == "" {
				return fmt.Errorf("cannot resolve token file path (home directory unknown)")
			}
			tok, err := auth.GenerateToken()
			if err != nil {
				return fmt.Errorf("generate token: %w", err)
			}
			if err := auth.WriteTokenFile(path, tok); err != nil {
				return fmt.Errorf("write token file %s: %w", path, err)
			}
			fmt.Fprintln(cmd.OutOrStdout(), tok)

			errOut := cmd.ErrOrStderr()
			fmt.Fprintf(errOut, "wrote new token to %s\n", path)
			if noRestart {
				fmt.Fprintf(errOut, "skipped restart (--no-restart); restart the warden daemon to apply the new token\n")
				return nil
			}
			desc, err := applyRotatedToken(tok)
			if err != nil {
				// The durable write succeeded, so this is a warning, not a hard
				// failure: the daemon still runs the OLD token until restarted.
				fmt.Fprintf(errOut, "warning: token written but daemon not restarted: %v\n", err)
				return nil
			}
			fmt.Fprintf(errOut, "%s — new token is live\n", desc)
			return nil
		},
	}
	cmd.Flags().BoolVar(&noRestart, "no-restart", false, "write the new token without restarting the daemon")
	return cmd
}
