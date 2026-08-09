package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/srjn45/warden/internal/client"
	"github.com/srjn45/warden/internal/config"
	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/tui"
)

func newTUICmd() *cobra.Command {
	var pane, agentPane, pipelineID, jobID, agentID string
	var repl, tmuxNative, killWindow, rebuildWebCockpit bool
	cmd := &cobra.Command{
		Use:   "tui",
		Short: "Live terminal cockpit for agents",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if rebuildWebCockpit {
				return rebuildWebCockpitSession()
			}
			a := clientFor(cmd)
			switch pane {
			case "control":
				return tui.RunControlPane(a, agentPane, killWindow)
			case "jobdetail":
				return tui.RunJobDetailPane(a, pipelineID, jobID)
			case "agentdetail":
				return tui.RunAgentDetailPane(a, agentID)
			case "":
				return runCockpit(a, cockpitUsesRepl(cmd, repl), cockpitTmuxNative(cmd, tmuxNative))
			default:
				return fmt.Errorf("unknown --pane %q (want control, jobdetail, or agentdetail)", pane)
			}
		},
	}
	cmd.Flags().BoolVar(&repl, "repl", false, "run the REPL (wd repl) in the terminal pane instead of a shell (default: repl config setting)")
	cmd.Flags().BoolVar(&tmuxNative, "tmux-native", false, "lay the cockpit out as a native tmux window in the current session instead of a nested tmux (auto-enabled when launched inside tmux; requires $TMUX)")
	cmd.Flags().StringVar(&pane, "pane", "", "internal: render a single cockpit pane (control, jobdetail, agentdetail)")
	cmd.Flags().StringVar(&agentPane, "agent-pane", "", "internal: tmux id of the agent pane the control pane drives")
	cmd.Flags().StringVar(&pipelineID, "pipeline", "", "internal: pipeline id for --pane=jobdetail")
	cmd.Flags().StringVar(&jobID, "job", "", "internal: job id for --pane=jobdetail")
	cmd.Flags().StringVar(&agentID, "agent", "", "internal: agent id for --pane=agentdetail")
	cmd.Flags().BoolVar(&killWindow, "kill-window", false, "internal: `q` kills only the cockpit window, not the session (tmux-native control pane)")
	cmd.Flags().BoolVar(&rebuildWebCockpit, "rebuild-web-cockpit", false, "kill and rebuild the daemon-owned web cockpit tmux session (the browser /tui view), then exit — an escape hatch for a wedged web cockpit")
	for _, f := range []string{"pane", "agent-pane", "pipeline", "job", "agent", "kill-window"} {
		_ = cmd.Flags().MarkHidden(f)
	}
	return cmd
}

// cockpitTmuxNative decides whether to launch the tmux-native cockpit (a window
// in the user's current session) rather than the classic own-session cockpit. An
// explicit --tmux-native flag always wins; otherwise it auto-enables when warden
// was launched from inside an existing tmux session, because the classic
// cockpit's `tmux attach` refuses to nest there. Mirrors cockpitUsesRepl.
func cockpitTmuxNative(cmd *cobra.Command, flagVal bool) bool {
	if f := cmd.Flags().Lookup("tmux-native"); f != nil && f.Changed {
		return flagVal
	}
	return tui.InsideTmux()
}

// cockpitUsesRepl decides which cockpit flavor to launch: the terminal pane runs
// the REPL when --repl is passed explicitly, otherwise it follows the `repl`
// config setting. An explicit flag always wins so both flavors stay launchable
// regardless of the default.
func cockpitUsesRepl(cmd *cobra.Command, flagVal bool) bool {
	if f := cmd.Flags().Lookup("repl"); f != nil && f.Changed {
		return flagVal
	}
	return config.Load(configPathFor(cmd)).GetRepl()
}

// runCockpit builds the tmux cockpit for this process and attaches to it. The
// control pane runs in the launching shell's directory so agents spawned from it
// (`n`) launch in that dir. useRepl selects the flavor whose terminal pane runs
// `wd repl` rather than a plain shell. When native is set, the cockpit is laid
// out as a window in the user's *current* tmux session (for when warden is
// launched from inside tmux, where the classic cockpit's `tmux attach` can't
// nest) instead of building its own session to attach to.
func runCockpit(a *client.Client, useRepl, native bool) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate warden binary: %w", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get working dir: %w", err)
	}
	if native {
		if !tui.InsideTmux() {
			return fmt.Errorf("--tmux-native requires running inside a tmux session (start tmux first, or drop the flag for the classic cockpit)")
		}
		fmt.Fprintln(os.Stderr, "warden: detected an existing tmux session — laying the cockpit out as a native tmux window (no nested tmux). For the classic full-screen cockpit, run: env -u TMUX wd tui")
		return tui.RunTmuxNativeCockpit(a, self, cwd)
	}
	return tui.RunCockpit(a, self, cwd, useRepl)
}

// rebuildWebCockpitSession forces a kill+rebuild of the daemon-owned web cockpit
// tmux session (the one the browser /tui view attaches to). The web cockpit lives
// in the user's shared tmux server rather than in the daemon, so a same-user CLI
// invocation can reset it directly — no daemon round-trip or endpoint needed —
// and the daemon's next attach finds the fresh, healthy session and reuses it.
// It matches the daemon's build parameters (home cwd, plain-shell terminal pane) so
// the rebuilt session is byte-for-byte what an attach would have produced.
func rebuildWebCockpitSession() error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate warden binary: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	run := lifecycle.HintingExecRunner{Inner: lifecycle.ExecRunner{}}
	if _, err := tui.EnsureWebCockpit(context.Background(), run, self, home, false, true); err != nil {
		return fmt.Errorf("rebuild web cockpit: %w", err)
	}
	fmt.Fprintln(os.Stdout, "warden: rebuilt the web cockpit session ("+tui.WebCockpitSession+"); reload the browser /tui view to reconnect.")
	return nil
}
