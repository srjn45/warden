package cli

import (
	"fmt"
	"os"

	"github.com/srajanpathak/agentctl/internal/client"
	"github.com/srajanpathak/agentctl/internal/store"
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
// every agentctl-spawned tmux session carries (AGENTCTL_SESSION_ID, set at
// `tmux new-session`). rotate is only meaningful from inside an agent.
func selfSessionID() (string, error) {
	id := os.Getenv("AGENTCTL_SESSION_ID")
	if id == "" {
		return "", fmt.Errorf("rotate must be run inside an agentctl agent session (AGENTCTL_SESSION_ID is unset)")
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
	if info.Size() == 0 {
		return fmt.Errorf("handoff file %q is empty", path)
	}
	return nil
}
