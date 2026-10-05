package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/spf13/cobra"
	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/srjn45/warden/internal/config"
	"github.com/srjn45/warden/internal/fastbrain"
	"github.com/srjn45/warden/internal/llm"
	"github.com/srjn45/warden/internal/memory"
	"github.com/srjn45/warden/internal/repl"
)

// newReplCmd builds `wd repl`: the natural-language REPL. It turns operator
// intent into confirmed warden tool calls via Fast-Brain; it never writes code
// (that is delegated to Claude agents) and confirms every mutation.
func newReplCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "repl",
		Aliases: []string{"interactive", "i"},
		Short:   "Interactive REPL for agents, pipelines, and the git/check lifecycle (Fast-Brain + `/` commands).",
		Long: `Interactive mode: a full-screen-free REPL to drive your warden fleet from the terminal.

It is a real line editor — arrow keys, history (persisted across sessions),
reverse-search, and Tab completion — that closes cleanly with Ctrl-D, returning
you to your shell prompt.

Two ways to drive it:
  • Deterministic ` + "`/` commands" + ` (no model): /agents, /spawn <prompt>, /tell <id> <text>,
    /memory <question>, /pipelines, … — typing / pops a live, filtering menu of verbs;
    Tab also completes verbs and live agent ids. Type /help for the list.
  • Natural language: any other line is planned by Fast-Brain into warden tool
    calls, each confirmed before it runs.

Guided argument forms: when a ` + "`/` command" + ` needs more than you typed, warden
collects the arguments interactively — a numbered pick-list for fields with a
known set (model, permission_mode, type, yes/no), free text for the rest. A
command auto-opens the form when a required argument is missing (e.g. bare
/spawn); add a trailing + to fill every field (/spawn+ <prompt>). Each field opens with a suggested value you can accept with Enter,
type over, or clear with "-".

` + "`!cmd`" + ` runs a command in your own $SHELL. Natural language runs on
Fast-Brain (headless backend CLI, no local model or config needed); the
` + "`/` commands" + ` and ` + "`!cmd`" + ` keep working if it is unavailable.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg := config.Load(configPathFor(cmd))
			out := cmd.OutOrStdout()
			cl := clientFor(cmd)
			// Natural language runs on Fast-Brain over a headless backend runner. It
			// fails open: with no runner, /commands and !shell still work.
			chat := fastbrain.NewFastBrainChatter(fastbrain.NewEngine(replRunner(), nil))
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
				repl.NewRouterFromConfig(cfg, nil),
			)
			// The orchestrator runs on top of the operator's own shell: `!`-lines
			// pass through to a persistent $SHELL started in the launch dir, teeing
			// output to the same terminal. A shell that won't start (no PTY) is not
			// fatal — `!` simply reports it's unavailable.
			cwd, _ := os.Getwd()
			// PR-3 (#53): local grounding of project questions from .warden/memory.md.
			// Read-only and $0 — it REMOVES cloud round-trips, so it is default on
			// (memory.ground). The grounder takes no model here, so it degrades to returning the matched entries
			// verbatim (still $0), never escalating to a paid model.
			if cfg.GetMemoryGround() {
				var comp llm.Completer
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

// replRunner returns the headless one-shot runner behind the REPL's Fast-Brain
// engine, or nil when the default backend has no headless mode (the chatter then
// fails open and only /commands and !shell are useful).
func replRunner() fastbrain.Runner {
	be := agentbackend.Default()
	if be == nil {
		return nil
	}
	return fastbrain.RunnerFunc(func(ctx context.Context, prompt string) (string, error) {
		argv, ok := be.HeadlessCmd(prompt)
		if !ok {
			return "", fmt.Errorf("backend %s has no headless mode", be.ID())
		}
		cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		out, err := exec.CommandContext(cctx, argv[0], argv[1:]...).Output()
		return string(out), err
	})
}
