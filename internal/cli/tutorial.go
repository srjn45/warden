package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/srjn45/warden/internal/config"
)

// tutorialMarker is the basename of the first-run completion marker, written
// next to the rest of warden's state in <data_dir>. Its presence suppresses the
// first-run hint; `wd tutorial --reset` removes it so the walkthrough runs fresh.
const tutorialMarker = "tutorial-complete"

// tutorialMarkerPath resolves the marker file inside the configured data dir.
func tutorialMarkerPath(dataDir string) string {
	return filepath.Join(dataDir, tutorialMarker)
}

// tutorialDone reports whether the completion marker exists. A stat error other
// than not-exist is treated as "not done" (fail toward showing the tutorial),
// matching the non-blocking spirit of the feature.
func tutorialDone(markerPath string) bool {
	_, err := os.Stat(markerPath)
	return err == nil
}

// writeTutorialMarker creates (or refreshes) the completion marker, recording a
// timestamp so the file is human-readable. It creates the parent data dir if it
// is missing (the daemon normally owns it, but the tutorial can run first).
func writeTutorialMarker(markerPath string) error {
	if dir := filepath.Dir(markerPath); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	body := "warden tutorial completed " + time.Now().UTC().Format(time.RFC3339) + "\n"
	return os.WriteFile(markerPath, []byte(body), 0o644)
}

// resetTutorialMarker removes the completion marker. It is idempotent: a missing
// marker is success, so `--reset` is safe to run repeatedly.
func resetTutorialMarker(markerPath string) error {
	err := os.Remove(markerPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// tutorialStep is one screen of the walkthrough: a short title, a sentence or
// two of copy, and the exact commands to try. Keeping the steps as plain data
// makes the engine trivially unit-testable and free of any TUI dependency.
type tutorialStep struct {
	Title    string
	Body     string
	Commands []string
}

// tutorialSteps returns the ordered walkthrough: the core loop (spawn → watch →
// talk → tear down) plus pointers to the two richer surfaces (TUI + web GUI).
// It assumes the daemon is already running (the install guide covers that).
func tutorialSteps() []tutorialStep {
	return []tutorialStep{
		{
			Title: "Welcome to warden",
			Body: "warden spawns, monitors, and tears down per-ticket Claude Code agent sessions.\n" +
				"This is a quick tour of the core loop. It changes nothing — every command\n" +
				"below is one you run yourself when you're ready. (Assumes the daemon is up;\n" +
				"if not, start it with `wd daemon` or the systemd service.)",
		},
		{
			Title: "1. Spawn an agent",
			Body: "Hand a task to a fresh agent. It lands in its own isolated git worktree so\n" +
				"its work never collides with yours or another agent's.",
			Commands: []string{`wd start --type fix "make the failing test pass"`},
		},
		{
			Title: "2. Watch your fleet",
			Body: "List every agent with its status (running, waiting-for-input, done). This is\n" +
				"your at-a-glance triage view.",
			Commands: []string{"wd ls", "wd status <id>"},
		},
		{
			Title: "3. Talk to an agent",
			Body: "Attach to drive an agent interactively, or send it a one-off message without\n" +
				"attaching (handy when it's waiting on input).",
			Commands: []string{"wd attach <id>", `wd send <id> "use the helper in utils.go"`},
		},
		{
			Title: "4. Tear it down",
			Body: "When the work is reviewed and merged, archive the agent. Its transcript stays\n" +
				"on disk for recovery; a clean worktree is reclaimed automatically.",
			Commands: []string{"wd done <id>"},
		},
		{
			Title: "5. The richer surfaces",
			Body: "Two ways to see everything at once: the cockpit TUI (run `wd` with no args)\n" +
				"and the web GUI the daemon serves. Both show the same live fleet.",
			Commands: []string{"wd tui", "wd daemon   # then open the web GUI it prints"},
		},
		{
			Title: "You're set",
			Body: "That's the loop: spawn → watch → talk → tear down. Run `wd <command> --help`\n" +
				"for any verb, `wd config` to see your settings, and `wd doctor` if something\n" +
				"looks off. Re-run this tour anytime with `wd tutorial --reset`.",
		},
	}
}

// renderTutorial writes every step to w. It is the "noninteractive" engine —
// the command prints all steps in one pass rather than blocking on a keypress,
// which keeps it safe for piping and trivially testable.
func renderTutorial(w io.Writer, steps []tutorialStep) {
	for i, s := range steps {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "%s\n", s.Title)
		if s.Body != "" {
			fmt.Fprintf(w, "%s\n", s.Body)
		}
		for _, c := range s.Commands {
			fmt.Fprintf(w, "    %s\n", c)
		}
	}
}

// tutorialHintLine is the single non-blocking nudge shown on a fresh first run.
const tutorialHintLine = "tip: new to warden? run `wd tutorial` for a 2-minute tour (or `wd tutorial --skip` to silence this)."

// tutorialHintSuppressedCmds are commands that must never carry the first-run
// hint: machine/automation contexts (daemon, mcp, hook/guard, completion) and
// full-screen or self-referential commands (the cockpit root, tui, the tutorial
// itself) where a stray line would be wiped or is redundant.
var tutorialHintSuppressedCmds = map[string]bool{
	"warden":     true, // root with no args → cockpit TUI
	"tui":        true,
	"tutorial":   true,
	"daemon":     true,
	"mcp":        true,
	"hook":       true,
	"guard":      true,
	"completion": true,
	"repl":       true, // interactive REPL
}

// shouldHintTutorial decides whether to emit the first-run hint. It shows ONLY
// when the marker is absent AND output is a real TTY AND the gate is on AND the
// invoked command isn't a suppressed (machine/full-screen) one. Any other case
// stays silent so automation, pipes, and the daemon/MCP surfaces are untouched.
func shouldHintTutorial(markerExists, isTTY, gateOn bool, cmdName string) bool {
	if markerExists || !isTTY || !gateOn {
		return false
	}
	return !tutorialHintSuppressedCmds[cmdName]
}

// maybeHintTutorial is the root pre-run side: best-effort, never errors, prints
// the hint to stderr (so it never corrupts a command's stdout, e.g. --json).
// TTY detection is keyed on stdout, per the feature's interactivity contract.
func maybeHintTutorial(cmd *cobra.Command) {
	cfg := config.Load(configPathFor(cmd))
	marker := tutorialMarkerPath(cfg.DataDir)
	if shouldHintTutorial(tutorialDone(marker), isTTY(cmd.OutOrStdout()), cfg.GetTutorial(), cmd.Name()) {
		fmt.Fprintln(cmd.ErrOrStderr(), tutorialHintLine)
	}
}

func newTutorialCmd() *cobra.Command {
	var reset, skip bool
	cmd := &cobra.Command{
		Use:   "tutorial",
		Short: "Run the first-run guided walkthrough of warden's core loop",
		Long: "A friendly, idempotent tour of warden: spawn → watch → talk → tear down, plus\n" +
			"the TUI and web GUI. Completing it (or --skip) writes a tutorial-complete marker\n" +
			"in your data_dir so the first-run hint stops nagging. Re-run with --reset to clear\n" +
			"the marker and see it fresh. Disable the hint entirely with the `tutorial` config\n" +
			"setting.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if reset && skip {
				return fmt.Errorf("--reset and --skip are mutually exclusive")
			}
			cfg := config.Load(configPathFor(cmd))
			marker := tutorialMarkerPath(cfg.DataDir)
			out := cmd.OutOrStdout()
			switch {
			case reset:
				if err := resetTutorialMarker(marker); err != nil {
					return err
				}
				fmt.Fprintf(out, "tutorial reset — the first-run hint will return; run `wd tutorial` to take the tour\n")
				return nil
			case skip:
				if err := writeTutorialMarker(marker); err != nil {
					return err
				}
				fmt.Fprintf(out, "tutorial skipped — marker written to %s\n", marker)
				return nil
			}
			renderTutorial(out, tutorialSteps())
			if err := writeTutorialMarker(marker); err != nil {
				return err
			}
			fmt.Fprintf(out, "\ntutorial complete — marker written to %s (re-run with --reset anytime)\n", marker)
			return nil
		},
	}
	cmd.Flags().BoolVar(&reset, "reset", false, "delete the completion marker so the tutorial (and first-run hint) run fresh")
	cmd.Flags().BoolVar(&skip, "skip", false, "mark the tutorial complete without running it (silences the first-run hint)")
	return cmd
}
