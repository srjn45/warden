package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/srjn45/warden/internal/config"
	"github.com/srjn45/warden/internal/llm"
	"github.com/srjn45/warden/internal/memory"
	"github.com/srjn45/warden/internal/repl"
)

// newReplCmd builds `wd repl`: the natural-language REPL. It turns operator
// intent into confirmed warden tool calls via a local LLM; it never writes code
// (that is delegated to Claude agents) and confirms every mutation.
func newReplCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "repl",
		Aliases: []string{"interactive", "i"},
		Short:   "Interactive REPL for agents, pipelines, and the git/check lifecycle (local LLM + `/` commands).",
		Long: `Interactive mode: a full-screen-free REPL to drive your warden fleet from the terminal.

It is a real line editor — arrow keys, history (persisted across sessions),
reverse-search, and Tab completion — that closes cleanly with Ctrl-D, returning
you to your shell prompt.

Two ways to drive it:
  • Deterministic ` + "`/` commands" + ` (no model): /agents, /spawn <prompt>, /tell <id> <text>,
    /memory <question>, /pipelines, … — typing / pops a live, filtering menu of verbs;
    Tab also completes verbs and live agent ids. Type /help for the list.
  • Natural language: any other line is planned by the local LLM into warden tool
    calls, each confirmed before it runs.

Guided argument forms: when a ` + "`/` command" + ` needs more than you typed, warden
collects the arguments interactively — a numbered pick-list for fields with a
known set (model, permission_mode, type, yes/no), free text for the rest. A
command auto-opens the form when a required argument is missing (e.g. bare
/spawn); add a trailing + to fill every field (/spawn+ <prompt>). With a local
model present each field opens with a suggested value you can accept with Enter,
type over, or clear with "-".

` + "`!cmd`" + ` runs a command in your own $SHELL. Requires local_llm: true for the
natural-language half; the ` + "`/` commands" + ` work regardless.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg := config.Load(configPathFor(cmd))
			out := cmd.OutOrStdout()
			// The deterministic `/` commands and `!shell` are the always-available
			// half, so interactive mode no longer hard-requires a local model — it
			// starts either way. Only the natural-language half needs local_llm; we
			// say so up front when it's off, then let surface() handle any bare line.
			if !cfg.GetLocalLLM() {
				fmt.Fprintln(out, "natural-language mode is off (local_llm.enabled: true not set in `wd config path`) — /commands and !shell still work; type /help.")
			}
			cl := clientFor(cmd)
			chat := llm.NewOllama(cfg.LocalLLM.URL, cfg.LocalLLM.Model, cfg.LocalLLMTimeoutDuration())
			// Show the operator what an empty model / permission_mode field in the
			// [e]dit flow will actually resolve to (warden fills these from config
			// when the model omits them).
			gate := repl.NewGate(cmd.InOrStdin(), cmd.OutOrStdout())
			gate.UseDefaults(map[string]string{
				"model":           cfg.GetModelDefault(),
				"permission_mode": cfg.GetDefaultPermissionMode(),
			})
			sess := repl.NewSession(
				chat, cl, repl.NewRegistry(),
				gate,
				repl.NewRouterFromConfig(cfg, chat),
			)
			// The orchestrator runs on top of the operator's own shell: `!`-lines
			// pass through to a persistent $SHELL started in the launch dir, teeing
			// output to the same terminal. A shell that won't start (no PTY) is not
			// fatal — `!` simply reports it's unavailable.
			cwd, _ := os.Getwd()
			// PR-3 (#53): local grounding of project questions from .warden/memory.md.
			// Read-only and $0 — it REMOVES cloud round-trips, so it is default on
			// (memory.ground). Wire the LOCAL model only when local_llm is enabled;
			// with none the grounder degrades to returning the matched entries
			// verbatim (still $0), never escalating to a paid model.
			if cfg.GetMemoryGround() {
				var comp llm.Completer
				if cfg.GetLocalLLM() {
					comp = chat // *llm.Ollama implements llm.Completer
				}
				sess.EnableGrounding(repl.NewGrounder(cwd, &memory.Store{}, comp))
			}
			var sh repl.ShellRunner
			if s, err := repl.NewShell(cwd, out); err == nil {
				defer s.Close()
				sh = s
			}
			return repl.RunREPL(cmd.Context(), sess, sh, cmd.InOrStdin(), out)
		},
	}
}
