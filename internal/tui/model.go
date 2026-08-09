package tui

import (
	"context"

	"github.com/srjn45/warden/internal/approval"
	"github.com/srjn45/warden/internal/client"
	"github.com/srjn45/warden/internal/digest"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/store"
)

// api is the subset of *client.Client the TUI needs (fakeable in tests).
type api interface {
	List(ctx context.Context) ([]*store.Session, error)
	Output(ctx context.Context, id string, lines int) (string, error)
	Spawn(ctx context.Context, p client.SpawnParams) (*store.Session, error)
	Terminate(ctx context.Context, id string) error
	Delete(ctx context.Context, id string, hard bool) error
	Input(ctx context.Context, id, text string) error
	SetName(ctx context.Context, id, name string) error
	ListDirs(ctx context.Context, path string) (client.DirListing, error)
	Approvals(ctx context.Context) (bool, []approval.View, error)
	Approve(ctx context.Context, id string, option int, fingerprint string) error
	PipelineList(ctx context.Context) ([]*pipeline.Pipeline, error)
	PipelineGet(ctx context.Context, id string) (*pipeline.Pipeline, error)
	PipelineRetry(ctx context.Context, pid, job string) error
	PipelinePause(ctx context.Context, pid string) error
	PipelineResume(ctx context.Context, pid string) error
	PipelineCancel(ctx context.Context, pid string) error
	PipelineDelete(ctx context.Context, pid string) error
	Digest(ctx context.Context, id string) (*digest.Digest, error)
	Pressure(ctx context.Context) (client.PressureStatus, error)
	CtxList(ctx context.Context, prefix string) ([]client.ContextEntry, error)
	MsgRecent(ctx context.Context, limit int) ([]client.Message, error)
	GetAutopilot(ctx context.Context) (client.AutopilotStatus, error)
	SetAutopilot(ctx context.Context, enabled bool, repo string) (client.AutopilotStatus, error)
	ListBackends(ctx context.Context) (client.BackendsState, error)
	RescanBackends(ctx context.Context) (client.BackendsState, error)
	SetBackendTier(ctx context.Context, id, tier string) (client.Backend, error)
	SetBackendEnabled(ctx context.Context, id string, enabled bool) (client.Backend, error)
	SetDefaultBackend(ctx context.Context, id string) (client.BackendsState, error)
	SetThinkingMode(ctx context.Context, mode string) (client.BackendSettings, error)
}

type mode int

const (
	modeNormal mode = iota
	modeNewAgent
	modeSendMsg
	modeConfirmKill
	modeHelp
	modeOpenDir               // path input for `o`
	modeNewAgentDir           // dir-override sub-state of modeNewAgent
	modeNewAgentName          // name-input sub-state of modeNewAgent
	modeNewAgentRole          // role-select sub-state of modeNewAgent
	modeNewAgentBackend       // backend-select sub-state of modeNewAgent
	modeRename                // edit the selected agent's name (from the details view)
	modeConfirmSpawn          // memory-pressure confirm before spawning
	modeConfirmDeletePipeline // y/N confirm before deleting a stopped pipeline
	modeInspector             // read-only shared-context + message-traffic view
	modeDigest                // scrollable completion digest for the selected agent
	modeApprovals             // answer pending tool-permission prompts
	modeDetails               // scrollable full detail view for the selected agent
	modeBackends              // agent-backend registry page (list, tier, default, enabled, thinking-mode)
	modeTerminalChoice        // `t`: (c)reate a terminal in the opened agent's dir or (f)ocus an existing one
)
