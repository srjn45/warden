package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newLLMCmd is the retired `warden llm` namespace, kept only so existing
// scripts do not hit "unknown command".
func newLLMCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "llm",
		Short: "Retired — Fast-Brain replaced the local LLM",
	}
	cmd.AddCommand(newLLMSuggestCmd())
	return cmd
}

// newLLMSuggestCmd is a compat stub: the local-model recommender is gone
// because the REPL and internal thinking now run on Fast-Brain.
func newLLMSuggestCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "suggest",
		Short: "Retired — Fast-Brain replaced local_llm",
		Long: `Retired. warden no longer runs a local model: the REPL (wd repl) and
internal thinking use Fast-Brain, so there is no local model to size or pull.
This command is kept for compatibility and exits 0.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), "retired — Fast-Brain replaced local_llm; no local model is needed.")
			return nil
		},
	}
}
