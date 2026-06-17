package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/srjn45/warden/internal/client"
	"github.com/srjn45/warden/internal/store"
)

// composeSuccessorPrompt builds the successor's initial prompt: it points the
// fresh agent at the handoff notes first, then appends the human-reviewed
// resume prompt.
func composeSuccessorPrompt(resumePrompt, handoffPath string) string {
	return fmt.Sprintf("You are resuming work handed off from a previous agent that is being retired. "+
		"First read the handoff notes at %s for full context, decisions already made, and next steps. "+
		"Then continue the work:\n\n%s", handoffPath, resumePrompt)
}

// buildSuccessorParams clones the retiring agent's launch configuration so the
// successor lands in the identical environment — same working directory (which,
// for a worktree-backed agent, IS the worktree dir) and the same supervised
// flag. It is a prompt-mode spawn (no Type/Repo/Worktree), so the successor
// reuses the existing worktree by cwd rather than creating a new one.
func buildSuccessorParams(old *store.Session, prompt string) client.SpawnParams {
	return client.SpawnParams{
		Prompt:     prompt,
		Cwd:        old.Workdir,
		Supervised: old.Supervised,
	}
}

// selfSessionID returns the current agent's own id from the environment that
// every warden-spawned tmux session carries (WARDEN_SESSION_ID, set at
// `tmux new-session`). rotate is only meaningful from inside an agent.
func selfSessionID() (string, error) {
	id := envID("SESSION_ID")
	if id == "" {
		return "", fmt.Errorf("rotate must be run inside a warden agent session (WARDEN_SESSION_ID is unset)")
	}
	return id, nil
}

// validateHandoff fails when the handoff file is missing or empty — caught
// before anything irreversible (spawn/reap) happens.
func validateHandoff(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("handoff file %q: %w", path, err)
	}
	if info.IsDir() {
		return fmt.Errorf("handoff file %q is a directory, not a file", path)
	}
	if info.Size() == 0 {
		return fmt.Errorf("handoff file %q is empty", path)
	}
	return nil
}

// rotator is the minimal daemon surface rotate needs. It deliberately OMITS
// RemoveWorktree: the successor inherits the live worktree by cwd, so rotate
// must never remove it — leaving the method off the interface makes that
// invariant a compile-time guarantee, not just a test.
type rotator interface {
	Get(ctx context.Context, id string) (*store.Session, error)
	Spawn(ctx context.Context, p client.SpawnParams) (*store.Session, error)
	Terminate(ctx context.Context, id string) error
}

// Client must satisfy rotator.
var _ rotator = (*client.Client)(nil)

// runRotate performs the irreversible half: spawn the successor in the retiring
// agent's environment, then reap the old agent. Ordering is spawn-before-reap
// and fail-safe — if Spawn fails, the old agent is NOT terminated (returns a nil
// successor), so no work is stranded. If the successor spawns but the reap
// fails, it returns the live successor AND a non-nil error so the caller can
// warn that the old agent may still be running.
//
// onSpawned (if non-nil) is invoked with the live successor AFTER the spawn
// succeeds but BEFORE the reap. This ordering is load-bearing for self-rotation:
// Terminate kills the very tmux session this process runs in, SIGKILLing the
// rotate process, so any user-facing summary must be emitted in onSpawned —
// printing it after runRotate returns would never be seen.
func runRotate(ctx context.Context, r rotator, selfID, successorPrompt string, onSpawned func(*store.Session)) (*store.Session, error) {
	old, err := r.Get(ctx, selfID)
	if err != nil {
		return nil, fmt.Errorf("look up self %q: %w", selfID, err)
	}
	successor, err := r.Spawn(ctx, buildSuccessorParams(old, successorPrompt))
	if err != nil {
		return nil, fmt.Errorf("spawn successor (old agent left running): %w", err)
	}
	if onSpawned != nil {
		onSpawned(successor)
	}
	if err := r.Terminate(ctx, selfID); err != nil {
		return successor, fmt.Errorf("successor %s spawned, but reaping old agent %s failed: %w", successor.ID, selfID, err)
	}
	return successor, nil
}

func newRotateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rotate",
		Short: "Hand this agent's work to a fresh successor in the same workspace, then retire it",
		Long: "Run inside an agent session. Phase 1 is driven by the /warden skill " +
			"(the agent writes a handoff file + resume prompt and shows you). On your " +
			"go-ahead, run with --confirm to spawn the successor and reap this agent.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			confirm, _ := cmd.Flags().GetBool("confirm")
			if !confirm {
				return fmt.Errorf("rotate is irreversible; re-run with --confirm once you've reviewed the handoff")
			}
			resumeFile, _ := cmd.Flags().GetString("resume-file")
			resumePrompt, _ := cmd.Flags().GetString("resume-prompt")
			if resumeFile == "" || resumePrompt == "" {
				return fmt.Errorf("--resume-file and --resume-prompt are both required with --confirm")
			}
			selfID, err := selfSessionID()
			if err != nil {
				return err
			}
			if err := validateHandoff(resumeFile); err != nil {
				return err
			}
			prompt := composeSuccessorPrompt(resumePrompt, resumeFile)
			out := cmd.OutOrStdout()
			// Summary is printed in onSpawned — BEFORE the reap — because the reap
			// kills this process in self-rotation. See runRotate's doc comment.
			onSpawned := func(successor *store.Session) {
				fmt.Fprintf(out, "rotated: successor %s spawned in %s\n", successor.ID, successor.Workdir)
				fmt.Fprintf(out, "  handoff notes: %s\n", resumeFile)
				fmt.Fprintf(out, "  old agent %s retiring; its transcript + the handoff file remain on disk for recovery\n", selfID)
				fmt.Fprintf(out, "  attach to the successor: warden attach %s\n", successor.ID)
			}
			successor, err := runRotate(cmd.Context(), clientFor(cmd), selfID, prompt, onSpawned)
			if successor == nil {
				return err // get/spawn failed; nothing irreversible happened, summary not printed
			}
			// Reaching here with a non-nil err means the reap failed — which means the
			// session was NOT killed, so this process is still alive to warn.
			if err != nil {
				fmt.Fprintf(out, "  WARNING: %v — check `warden ls`; if it's still running, retry `warden done %s` or attach and /exit\n", err, selfID)
			}
			return nil
		},
	}
	cmd.Flags().Bool("confirm", false, "actually spawn the successor and retire this agent (required)")
	cmd.Flags().String("resume-file", "", "path to the handoff notes file the successor should read")
	cmd.Flags().String("resume-prompt", "", "the successor's initial task prompt")
	return cmd
}
