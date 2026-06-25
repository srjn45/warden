package cli

import (
	"errors"
	"os"

	"github.com/spf13/cobra"
	"github.com/srjn45/warden/internal/config"
	"github.com/srjn45/warden/internal/llm"
	"github.com/srjn45/warden/internal/orchestrator"
)

// newOrchCmd builds `wd orch`: the natural-language conductor REPL. It turns
// operator intent into confirmed warden tool calls via a local LLM; it never
// writes code (that is delegated to Claude agents) and confirms every mutation.
func newOrchCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "orch",
		Aliases: []string{"orchestrator"},
		Short:   "Natural-language conductor for agents, pipelines, and the git/check lifecycle (local LLM).",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg := config.Load(configPathFor(cmd))
			if !cfg.GetLocalLLM() {
				return errors.New("orchestrator requires local_llm: true (it is an interactive surface with no deterministic fallback) — set it with `wd config set local_llm true`")
			}
			cl := clientFor(cmd)
			chat := llm.NewOllama(cfg.LocalLLMURL, cfg.LocalLLMModel, cfg.LocalLLMTimeoutDuration())
			sess := orchestrator.NewSession(
				chat, cl, orchestrator.NewRegistry(),
				orchestrator.NewGate(cmd.InOrStdin(), cmd.OutOrStdout()),
				orchestrator.NewRouterFromConfig(cfg),
			)
			// The orchestrator runs on top of the operator's own shell: `!`-lines
			// pass through to a persistent $SHELL started in the launch dir, teeing
			// output to the same terminal. A shell that won't start (no PTY) is not
			// fatal — `!` simply reports it's unavailable.
			out := cmd.OutOrStdout()
			cwd, _ := os.Getwd()
			var sh orchestrator.ShellRunner
			if s, err := orchestrator.NewShell(cwd, out); err == nil {
				defer s.Close()
				sh = s
			}
			return orchestrator.RunREPL(cmd.Context(), sess, sh, cmd.InOrStdin(), out)
		},
	}
}
