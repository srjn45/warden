package daemon

import (
	"context"
	"errors"
	"log/slog"

	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/store"
)

// Pipeline ownership-edge maintenance (docs/specs/2026-09-25-project-entity-hierarchy.md
// D4/D6/§6.1). A pipeline's owning-agent relationship is stored on BOTH ends: the
// pipeline's ParentAgentID back-ref and the owning agent's ChildPipelines[]
// forward edge. These helpers keep the forward edge consistent at the two write
// edges this phase owns — pipeline create/escalate (add) and pipeline delete
// (remove). The owning agent's ChildPipelines[] is the edge of record when the
// two ends disagree (§6.1); the per-pipeline ParentAgentID is the mirror.
//
// A pipeline's own JOB agents are NOT owned this way (D5/§6.2): they carry
// PipelineID and belong to the pipeline (reached via ChildPipelines[] ->
// Pipeline.jobs), never a parent agent's ChildAgents[]. Only the pipeline itself
// is attributed to its owning agent, once, here.

// resolvePipelineParentAgentID returns the id of the agent behind a pipeline
// create request — the actor identity from the request's actor header — as the
// pipeline's owning agent, the pipeline-side mirror of the spawn-time parent_id.
// The caller has already applied the higher-precedence source (an explicit
// request-body parent_agent_id); this only fills the still-empty case from the
// live actor. It returns "" for an operator create (no actor header, or an
// unknown/stale id) and for a terminal caller — a terminal is a leaf member and
// never owns a pipeline (§6.4). A pipeline so created is operator-owned and joins
// no agent's ChildPipelines[].
func (s *Server) resolvePipelineParentAgentID(ctx context.Context) string {
	caller := s.callerSession(ctx)
	if caller == nil || caller.IsTerminal() {
		return ""
	}
	return caller.ID
}

// addPipelineParentEdge appends a created pipeline to its owning agent's
// authoritative ChildPipelines[] forward edge (spec D4/§6.1), the pipeline
// analogue of addChildEdge. Best-effort: the append de-duplicates (a repeat is a
// no-op, so re-create is idempotent) and a failure is logged, never fatal — the
// pipeline keeps its ParentAgentID back-ref regardless, and the two ends are
// reconciled with this list as the source of truth. An operator-created pipeline
// (empty ParentAgentID) is a silent no-op, and a missing owning agent is
// tolerated (dangling back-ref, §6.3). Call AFTER a successful pstore.Create.
func (s *Server) addPipelineParentEdge(ctx context.Context, p *pipeline.Pipeline) {
	if p == nil || p.ParentAgentID == "" {
		return
	}
	if err := s.store.Update(ctx, p.ParentAgentID, func(a *store.Session) error {
		a.ChildPipelines = appendUnique(a.ChildPipelines, p.ID)
		return nil
	}); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return // owning agent gone (dangling back-ref tolerated, §6.3)
		}
		slog.Warn("daemon: pipeline parent edge: add failed", "pipeline", p.ID, "agent", p.ParentAgentID, "err", err)
	}
}

// removePipelineParentEdge drops a deleted pipeline from its owning agent's
// ChildPipelines[] list, the delete-side mirror of addPipelineParentEdge.
// Best-effort (removing an absent id is a no-op) and a silent no-op for an
// operator-created pipeline (empty ParentAgentID). A missing owning agent is
// tolerated (§6.3). Call when a pipeline is deleted.
func (s *Server) removePipelineParentEdge(ctx context.Context, p *pipeline.Pipeline) {
	if p == nil || p.ParentAgentID == "" {
		return
	}
	if err := s.store.Update(ctx, p.ParentAgentID, func(a *store.Session) error {
		a.ChildPipelines = removeString(a.ChildPipelines, p.ID)
		return nil
	}); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return
		}
		slog.Warn("daemon: pipeline parent edge: remove failed", "pipeline", p.ID, "agent", p.ParentAgentID, "err", err)
	}
}
