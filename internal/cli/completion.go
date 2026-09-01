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
	SetCommandHelpMetadata(cmd, "operate", 20, "warden completion", "", NodeNamespace)

	shells := []struct {
		name, short string
		gen         func(*cobra.Command, []string) error
	}{
		{"bash", "Generate bash completion script", func(c *cobra.Command, _ []string) error { return c.Root().GenBashCompletion(os.Stdout) }},
		{"zsh", "Generate zsh completion script", func(c *cobra.Command, _ []string) error { return c.Root().GenZshCompletion(os.Stdout) }},
		{"fish", "Generate fish completion script", func(c *cobra.Command, _ []string) error { return c.Root().GenFishCompletion(os.Stdout, true) }},
		{"powershell", "Generate PowerShell completion script", func(c *cobra.Command, _ []string) error { return c.Root().GenPowerShellCompletion(os.Stdout) }},
	}
	for i, shell := range shells {
		child := &cobra.Command{
			Use:   shell.name,
			Short: shell.short,
			RunE:  shell.gen,
		}
		SetCommandHelpMetadata(child, "operate", (i+1)*10, "warden completion "+shell.name, "", NodeLeaf)
		cmd.AddCommand(child)
	}
	return cmd
}
