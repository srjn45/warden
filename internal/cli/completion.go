package cli

import (
	"os"

	"github.com/spf13/cobra"
)

const completionLong = `Generate shell completion scripts for warden.

The completion script for each shell should be redirected to the appropriate
location for your shell. Examples:

Bash:
  warden completion bash > /etc/bash_completion.d/warden
  # or for user-only installation:
  warden completion bash > ~/.bash_completion

Zsh:
  warden completion zsh > /usr/local/share/zsh/site-functions/_warden
  # or for user-only installation:
  warden completion zsh > ~/.zsh/completion/_warden

Fish:
  warden completion fish > ~/.config/fish/completions/warden.fish

PowerShell:
  warden completion powershell > warden.ps1
  # Then load it in your PowerShell profile

After generating the completion script, you may need to restart your shell
or source the file for the completions to take effect.
`

func newCompletionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "completion",
		Short: "Generate shell completion scripts",
		Long:  completionLong,
	}

	cmd.AddCommand(&cobra.Command{
		Use:   "bash",
		Short: "Generate bash completion script",
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Root().GenBashCompletion(os.Stdout)
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "zsh",
		Short: "Generate zsh completion script",
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Root().GenZshCompletion(os.Stdout)
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "fish",
		Short: "Generate fish completion script",
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Root().GenFishCompletion(os.Stdout, true)
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "powershell",
		Short: "Generate PowerShell completion script",
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Root().GenPowerShellCompletion(os.Stdout)
		},
	})

	return cmd
}
