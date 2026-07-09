package daemon

import (
	"context"
	"net/http"
	"strings"

	"github.com/srjn45/warden/internal/auth"
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
