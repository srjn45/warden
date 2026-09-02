package daemon

import (
	"context"
	"net/http"
	"slices"
	"strings"

	"github.com/srjn45/warden/internal/auth"
	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/store"
)

// autopilotOwnershipTag is the tag every autopilot-owned agent (brain + workers)
// carries — the daemon-side mirror of autopilot.autopilotTag. The ownership guard
// requires it on a target before a brain may act on it.
const autopilotOwnershipTag = "autopilot"

// runTagPrefix prefixes the per-run ownership tag (`run:<run_id>`) that a brain
// and its workers share — the daemon-side mirror of autopilot.runTag.
const runTagPrefix = "run:"

// callerSession resolves the authenticated agent session behind a request from
// its actor header (auth.ActorHeader, set by the client when it runs inside an
// agent shell). It returns nil when the header is absent or names no known
// session — a human terminal, the web UI, or a stale id — in which case the
// ownership guard is a no-op and normal behavior applies.
func (s *Server) callerSession(ctx context.Context) *store.Session {
	id := strings.TrimSpace(requestFromContext(ctx).Header.Get(auth.ActorHeader))
	if id == "" {
		return nil
	}
	sess, err := s.store.GetByNameOrID(ctx, id)
	if err != nil {
		return nil
	}
	return sess
}

// guardOwnership enforces autopilot's ownership fence (docs/specs/autopilot.md
// §8) on a destructive operation. When the CALLER is a run's brain (role
// autopilot), the TARGET must carry the `autopilot` tag and the caller's own
// `run:<run_id>` tag; otherwise the call is refused with 403 not_owned. So a
// brain is mechanically confined to the agents of its own run — it cannot
// terminate, delete, remove-worktree, or snapshot-restore a manual agent, a
// foreign run's worker, or the human's own work, regardless of what the persona
// decides. A non-autopilot caller (a human, the web UI, an ordinary agent) is
// unaffected: the guard returns nil and the handler proceeds as before.
func (s *Server) guardOwnership(ctx context.Context, target *store.Session) error {
	caller := s.callerSession(ctx)
	if caller == nil || caller.Role != autopilotBrainRole {
		return nil // not a brain — normal behavior, guard is a no-op
	}
	if target == nil {
		return nil // nothing to guard (target resolution already errored out)
	}
	// A brain may always act on itself (rotation / self-teardown by the guardian).
	if target.ID == caller.ID {
		return nil
	}
	runTag := callerRunTag(caller)
	if runTag == "" || !target.HasTag(autopilotOwnershipTag) || !target.HasTag(runTag) {
		return errStatus(http.StatusForbidden, "not_owned: target agent is not owned by this autopilot run")
	}
	return nil
}

// callerRunTag returns the brain's `run:<run_id>` ownership tag, or "" when it
// carries none — in which case it owns nothing and guardOwnership denies every
// foreign target (the safe default).
func callerRunTag(caller *store.Session) string {
	for _, t := range caller.Tags {
		if strings.HasPrefix(t, runTagPrefix) {
			return t
		}
	}
	return ""
}

// inheritOwnershipTags extends autopilot's ownership tags (`autopilot` +
// `run:<run_id>`) from the calling agent onto the tags of an agent it is
// creating. When the caller behind a spawn (or pipeline-create) request is
// itself autopilot-owned — the run's manager, or transitively one of its
// workers — the new agent joins the same run whether or not the caller's
// persona remembered to pass the tags. This keeps the overwatch roster and the
// §8 ownership fence complete mechanically instead of by prompt discipline
// (docs/specs/autopilot.md §2.4). Any other caller — a human terminal, the web
// UI, an ordinary agent — gets its tags back untouched.
func (s *Server) inheritOwnershipTags(ctx context.Context, tags []string) []string {
	caller := s.callerSession(ctx)
	if caller == nil || !caller.HasTag(autopilotOwnershipTag) {
		return tags
	}
	rt := callerRunTag(caller)
	if rt == "" {
		return tags // no run identity to inherit
	}
	for _, t := range []string{autopilotOwnershipTag, rt} {
		if !slices.Contains(tags, t) {
			tags = append(tags, t)
		}
	}
	return tags
}

func isAutopilotWorkerSpawnRole(role string) bool {
	return autopilot.WorkerSpawnRole(role)
}

// stampAutopilotSpawnBackRefs sets explicit run back-ref fields on worker spawns
// from an autopilot-owned caller and clears parent_id — autopilot workers are
// grouped by back-ref, not a live parent chain (plan-scoped hierarchy WP6).
func (s *Server) stampAutopilotSpawnBackRefs(ctx context.Context, sr *SpawnRequest) {
	if sr == nil {
		return
	}
	caller := s.callerSession(ctx)
	if caller == nil || !caller.HasTag(autopilotOwnershipTag) {
		return
	}
	if !isAutopilotWorkerSpawnRole(sr.Role) {
		return
	}
	runID := strings.TrimPrefix(callerRunTag(caller), runTagPrefix)
	if runID == "" {
		runID = autopilot.SessionRunID(caller)
	}
	if runID == "" {
		return
	}
	sr.ParentID = ""
	sr.AutopilotRunID = runID
	sr.AutopilotSlot = store.AutopilotSlotWorker
	sr.AutopilotTaskID = strings.TrimSpace(sr.Task)
}

// annotateAutopilotWorkerPrompt appends the resolved integration branch to a
// worker spawn prompt so workers do not guess the PR base. No-op for non-worker
// roles, non-autopilot callers, or when the branch is already in the prompt.
func (s *Server) annotateAutopilotWorkerPrompt(ctx context.Context, sr *SpawnRequest) {
	if sr == nil || !isAutopilotWorkerSpawnRole(sr.Role) {
		return
	}
	caller := s.callerSession(ctx)
	if caller == nil || !caller.HasTag(autopilotOwnershipTag) {
		return
	}
	runID := strings.TrimPrefix(callerRunTag(caller), runTagPrefix)
	if runID == "" || s.autopilot == nil {
		return
	}
	lp, ok := s.autopilot.LandParams(runID)
	if !ok {
		return
	}
	sr.Prompt = autopilot.AppendWorkerSpawnBranch(sr.Prompt, lp.IntegrationBranch)
}
