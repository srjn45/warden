// Package orchestrator is warden's natural-language conductor: a local-LLM
// REPL that turns operator intent into *confirmed* warden tool calls (spawn /
// monitor / tear down agents, drive pipelines, run the git/check lifecycle). It
// is a thin translator, not a brain — it never writes code (that is delegated to
// Claude agents via spawn_agent) and every mutation is rendered and confirmed
// before it runs. The package is a second front-end onto the same
// *client.Client the MCP server wraps; it adds no business logic of its own.
package orchestrator

import (
	"context"

	"github.com/srjn45/warden/internal/approval"
	"github.com/srjn45/warden/internal/client"
	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/store"
)

// Daemon is the subset of *client.Client the orchestrator drives. *client.Client
// satisfies it as-is — the orchestrator is a second front-end onto the same
// client the MCP server uses, never a reimplementation. Reads and mutations both
// live here; the read/mutate split is expressed per-tool in the registry, which
// is what the confirm gate keys off — not this interface.
type Daemon interface {
	// reads
	List(ctx context.Context) ([]*store.Session, error)
	Get(ctx context.Context, id string) (*store.Session, error)
	Output(ctx context.Context, id string, lines int) (string, error)
	Approvals(ctx context.Context) (bool, []approval.View, error)
	MsgInbox(ctx context.Context, id string, unreadOnly bool) ([]client.Message, error)
	CtxGet(ctx context.Context, key string) (client.ContextEntry, error)
	CtxList(ctx context.Context, prefix string) ([]client.ContextEntry, error)
	PipelineList(ctx context.Context) ([]*pipeline.Pipeline, error)
	PipelineGet(ctx context.Context, id string) (*pipeline.Pipeline, error)
	CollabConflicts(ctx context.Context) ([]client.Conflict, error)
	// mutations
	Spawn(ctx context.Context, p client.SpawnParams) (*store.Session, error)
	Input(ctx context.Context, id, text string) error
	Terminate(ctx context.Context, id string) error
	Delete(ctx context.Context, id string, hard bool) error
	Restore(ctx context.Context, id string) error
	Approve(ctx context.Context, id string, option int, fingerprint string) error
	GitCommit(ctx context.Context, session, dir, message string) (lifecycle.CommitResult, error)
	GitPush(ctx context.Context, session, dir string) (lifecycle.PushResult, error)
	GitSync(ctx context.Context, session, dir, base string) (lifecycle.SyncResult, error)
	Check(ctx context.Context, session, dir, name string) (lifecycle.CheckResult, error)
	CtxSet(ctx context.Context, key, value, by string) (client.ContextEntry, error)
	MsgSend(ctx context.Context, to, from, body string) (client.Message, bool, error)
	PipelineCreate(ctx context.Context, specYAML string) (*pipeline.Pipeline, error)
	PipelineCancel(ctx context.Context, id string) error
}

var _ Daemon = (*client.Client)(nil)
