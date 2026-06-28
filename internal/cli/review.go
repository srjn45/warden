package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"

	"github.com/spf13/cobra"

	"github.com/srjn45/warden/internal/agentbackend"
	_ "github.com/srjn45/warden/internal/agentbackend/backends" // register the adapters so agentbackend.Get resolves in-process
)

// reviewBackend resolves the agent.Backend this `wd review` runs for. An explicit
// --backend wins (review a chosen backend, or run outside an agent); otherwise the
// owning agent's WARDEN_SESSION_ID is looked up (an existing read — no new daemon
// surface) and its recorded backend is used; failing both it falls through to the
// default backend, which surfaces a clear "no native review" degrade. It is a
// package var so tests can stub the session lookup without a live daemon.
var reviewBackend = func(cmd *cobra.Command, session, override string) (agentbackend.Backend, error) {
	id := override
	if id == "" && session != "" {
		if s, err := clientFor(cmd).Get(context.Background(), session); err == nil && s != nil {
			id = s.Backend
		}
	}
	return agentbackend.Get(id)
}

func newReviewCmd() *cobra.Command {
	var base, prompt, backend string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "review",
		Short: "Run the agent backend's native diff review on the worktree",
		Long: "Ask this agent's backend to review its own diff — the agent-native counterpart\n" +
			"to `wd check`. Where `wd check` runs the project's configured test/lint commands\n" +
			"and `pr-review` stands up a whole reviewer session, `wd review` invokes the\n" +
			"backend's OWN one-shot reviewer (Codex: `codex review`) against the worktree and\n" +
			"streams its findings to you — additive and on-top, no review session to manage.\n\n" +
			"By default it reviews the uncommitted working tree (staged + unstaged +\n" +
			"untracked); pass --base <branch> to review the branch's changes against a base\n" +
			"instead. The review runs locally in the agent's worktree (like a check); the\n" +
			"model/provider comes from the backend's own config, so the $0-local Ollama rig\n" +
			"and a paid setup both work unchanged.\n\n" +
			"Pass --json for a machine-readable result: warden runs the backend's structured\n" +
			"review (Codex: `codex exec review`), normalizes the backend's NATIVE review\n" +
			"output into a neutral findings shape ({summary, verdict, findings[]}), and prints\n" +
			"that JSON to stdout (the backend's own progress goes to stderr). Note: review\n" +
			"quality rides the backend's configured model — a tiny local model may report no\n" +
			"findings; the operator's real model is where this earns its keep.\n\n" +
			"Backends without a native review (e.g. Claude) are not offered the verb — it\n" +
			"exits non-zero pointing you at `wd check` or a `pr-review` agent.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, session := gitTarget()
			be, err := reviewBackend(cmd, session, backend)
			if err != nil {
				return err
			}
			rv, ok := be.(agentbackend.Reviewer)
			if !ok {
				return fmt.Errorf("backend %q has no native review; use `wd check` or a `pr-review` agent", be.ID())
			}

			opts := agentbackend.ReviewOpts{Scope: "uncommitted", Prompt: prompt, Structured: asJSON}
			if base != "" {
				opts.Scope = "base"
				opts.Base = base
			}
			argv, ok := rv.ReviewCmd(opts)
			if !ok || len(argv) == 0 {
				return fmt.Errorf("backend %q has no native review; use `wd check` or a `pr-review` agent", be.ID())
			}

			if asJSON {
				return runStructuredReview(cmd, be, dir, argv)
			}

			// Exec the review directly in the worktree and stream it to the operator —
			// CLI-local, like a check; no daemon round-trip. argv is a real argv (no
			// shell), so untrusted values never reach a shell.
			rc := exec.CommandContext(cmd.Context(), argv[0], argv[1:]...)
			rc.Dir = dir
			rc.Stdin = cmd.InOrStdin()
			rc.Stdout = cmd.OutOrStdout()
			rc.Stderr = cmd.ErrOrStderr()
			if err := rc.Run(); err != nil {
				if isCommandNotFound(err) {
					return fmt.Errorf("%s not found — %s", be.Binary(), installHint(be.Binary()))
				}
				return err
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&base, "base", "", "review changes against this base branch (default: the uncommitted working tree)")
	cmd.Flags().StringVar(&prompt, "prompt", "", "optional extra review instructions for the backend's reviewer")
	cmd.Flags().StringVar(&backend, "backend", "", "review for this backend id (default: the current agent's backend)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable findings (neutral JSON) instead of streaming the prose review")
	return cmd
}

// runStructuredReview drives the `--json` path: it runs the backend's structured review
// form in the worktree (its own output to stderr so stdout stays clean), then reads the
// backend's NATIVE structured result back via StructuredReviewer and prints it as the
// neutral findings JSON. CLI-local, like the prose path — no daemon round-trip.
func runStructuredReview(cmd *cobra.Command, be agentbackend.Backend, dir string, argv []string) error {
	sr, ok := be.(agentbackend.StructuredReviewer)
	if !ok {
		return fmt.Errorf("backend %q has no structured review; run `wd review` without --json", be.ID())
	}

	// argv is a real argv (no shell). Send the backend's own progress to stderr so the
	// operator still sees it while stdout carries only the neutral JSON.
	rc := exec.CommandContext(cmd.Context(), argv[0], argv[1:]...)
	rc.Dir = dir
	rc.Stdin = cmd.InOrStdin()
	rc.Stdout = cmd.ErrOrStderr()
	rc.Stderr = cmd.ErrOrStderr()
	if err := rc.Run(); err != nil {
		if isCommandNotFound(err) {
			return fmt.Errorf("%s not found — %s", be.Binary(), installHint(be.Binary()))
		}
		return err
	}

	findings, ok, err := sr.ParseReviewResult(dir)
	if err != nil {
		return fmt.Errorf("read structured review output: %w", err)
	}
	if !ok {
		return fmt.Errorf("%s produced no structured review output (the model may have emitted none); run `wd review` for the prose review", be.ID())
	}

	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(findings)
}
