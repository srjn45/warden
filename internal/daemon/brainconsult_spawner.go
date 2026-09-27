package daemon

import (
	"context"
	"fmt"

	"github.com/srjn45/warden/internal/brainconsult"
	"github.com/srjn45/warden/internal/store"
)

// brainConsultSpawner adapts Server's Lifecycle + Store to brainconsult.Spawner.
// It mirrors the spawn → insert → rollback-on-insert-failure flow the HTTP spawn
// handler and autopilotRuntime.SpawnBrain use, keeping brainconsult import-cycle
// free of daemon types.
type brainConsultSpawner struct {
	life  Lifecycle
	store store.Store
}

// Ensure the adapter satisfies the Spawner interface at compile time.
var _ brainconsult.Spawner = brainConsultSpawner{}

// Spawn launches a headless brain agent and persists it. On insert failure the
// tmux session is torn down so we never leak an untracked agent.
func (a brainConsultSpawner) Spawn(ctx context.Context, args brainconsult.BrainSpawnArgs) (*store.Session, error) {
	if a.life == nil {
		return nil, fmt.Errorf("brain consult: lifecycle not configured")
	}
	req := SpawnRequest{
		Cwd:     args.Cwd,
		Repo:    args.Repo,
		Prompt:  args.Prompt,
		Role:    args.Role,
		Backend: args.Backend,
		Tags:    args.Tags,
	}
	sess, err := a.life.Spawn(ctx, req)
	if err != nil {
		return nil, err
	}
	if a.store == nil {
		return sess, nil
	}
	if err := a.store.Insert(ctx, sess); err != nil {
		tctx, cancel := context.WithTimeout(context.Background(), brainTeardownTimeout)
		defer cancel()
		_ = a.life.Teardown(tctx, sess)
		return nil, fmt.Errorf("brain consult: persist: %w", err)
	}
	return sess, nil
}

// Output returns the last n lines of the agent's pane.
func (a brainConsultSpawner) Output(ctx context.Context, tmuxSession string, lines int) (string, error) {
	if a.life == nil {
		return "", fmt.Errorf("brain consult: lifecycle not configured")
	}
	return a.life.Output(ctx, tmuxSession, lines)
}

// Teardown force-kills the agent's tmux session without touching the store
// (Consultor's defer always runs this).
func (a brainConsultSpawner) Teardown(ctx context.Context, sess *store.Session) error {
	if a.life == nil {
		return fmt.Errorf("brain consult: lifecycle not configured")
	}
	return a.life.Teardown(ctx, sess)
}
