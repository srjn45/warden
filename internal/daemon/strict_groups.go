package daemon

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/daemon/oapi"
	"github.com/srjn45/warden/internal/groupstore"
	"github.com/srjn45/warden/internal/role"
	"github.com/srjn45/warden/internal/store"
)

// JoinGroup implements POST /api/v1/collaborate/groups/{name}/join (docs/specs/
// 2026-08-26-collaboration-groups.md §3.1/§4.1). It seats the calling agent in
// the named group — creating the group if absent — keyed by its project (the
// canonical git remote of the agent's worktree, or a `local:` path fallback).
// One orchestrator per project is enforced: if a DIFFERENT agent already holds a
// seat for this project key, join fails with 409 returning the incumbent so the
// caller can message it. The same agent re-joining its own seat is idempotent
// (its seat timestamp is refreshed). On a successful seat the caller is switched
// to the orchestrator role. Introductions and summary resolution arrive in later
// stages; this stage only seats/creates/flips-role and rejects duplicates.
func (s *Server) JoinGroup(ctx context.Context, req oapi.JoinGroupRequestObject) (oapi.JoinGroupResponseObject, error) {
	if s.groups == nil {
		return nil, errStatus(http.StatusServiceUnavailable, "collaboration groups unavailable")
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return nil, errStatus(http.StatusBadRequest, "group name required")
	}
	agentRef := ""
	if req.Body != nil {
		agentRef = strings.TrimSpace(req.Body.AgentId)
	}
	if agentRef == "" {
		return nil, errStatus(http.StatusBadRequest, "agent_id required")
	}
	sess, err := s.resolveSession(ctx, agentRef)
	if err != nil {
		return nil, err
	}
	projectKey := ProjectKeyForDir(ctx, sessionRepoDir(sess))

	// Resolve the project summary before seating (design §4.2): declared blurb
	// beats cached beats agent-generated-once. We load any existing group record
	// here to consult its SummaryCache; the group may not exist yet for a fresh
	// create, in which case preGrp is nil and only the file-read path runs.
	var preGrp *groupstore.Group
	if existing, gErr := s.groups.Get(name); gErr == nil {
		preGrp = existing
	}
	resolvedSummary := resolveGroupSummary(ctx, s, sess, preGrp, name, projectKey)

	member := groupstore.Member{
		AgentID:    sess.ID,
		ProjectKey: projectKey,
		Summary:    resolvedSummary,
		JoinedAt:   time.Now().UTC(),
	}

	// Seat the agent atomically, enforcing one-orchestrator-per-project. A fresh
	// group has no possible conflict, so the create path seats directly; an
	// existing group is mutated under the store lock so the conflict check and the
	// seat cannot race a concurrent join. seated tracks whether this call added a
	// NEW seat (vs an idempotent re-join of the agent's own seat) so introductions
	// are brokered exactly once per real join, not on every idempotent poll.
	var conflict *groupstore.Member
	seated := false
	cerr := s.groups.Create(&groupstore.Group{Name: name, Members: []groupstore.Member{member}})
	switch {
	case cerr == nil:
		// created + seated
		seated = true
	case errors.Is(cerr, groupstore.ErrExists):
		if uerr := s.groups.Update(name, func(g *groupstore.Group) {
			for i := range g.Members {
				if g.Members[i].ProjectKey != projectKey {
					continue
				}
				if g.Members[i].AgentID == sess.ID {
					// Idempotent re-join of the same agent's own seat.
					g.Members[i].JoinedAt = member.JoinedAt
					return
				}
				incumbent := g.Members[i]
				conflict = &incumbent
				return
			}
			g.Members = append(g.Members, member)
			seated = true
		}); uerr != nil {
			if errors.Is(uerr, groupstore.ErrNotFound) {
				// Racy delete between Create and Update; the group is gone again.
				return nil, errStatus(http.StatusNotFound, "group not found")
			}
			return nil, uerr
		}
	default:
		return nil, cerr
	}
	if conflict != nil {
		return oapi.JoinGroup409JSONResponse{
			Error:     "project already seated by agent " + conflict.AgentID,
			Incumbent: toOAPIMember(*conflict),
		}, nil
	}

	// Persist the resolved summary in the group's SummaryCache so it survives a
	// leave/rejoin cycle (the member record is removed on leave, but the cache
	// is not). Best-effort: a write failure never fails the join itself.
	if resolvedSummary != "" && seated {
		_ = s.groups.CacheSummary(name, projectKey, resolvedSummary)
	}

	// Switch the seated agent to the orchestrator role (best-effort relaunch;
	// persisted regardless per applyRole). Only after a successful seat so a
	// rejected (409) joiner is never flipped.
	orch, _ := role.Get("orchestrator")
	if err := s.applyRole(ctx, sess, orch.Name); err != nil {
		return nil, err
	}

	grp, err := s.groups.Get(name)
	if err != nil {
		return nil, err
	}
	// Broker introductions in both directions (design §3.2), but only on a real
	// new seat — an idempotent re-join must not re-announce. Warden composes and
	// delivers every message here, so the joining agent spends no tokens.
	if seated {
		s.brokerIntroductions(ctx, grp, member, sess.Name)
	}
	s.recordAuditCtx(ctx, "group.join", name, map[string]string{"agent": sess.ID, "project": projectKey})
	s.notify()
	return oapi.JoinGroup200JSONResponse{Group: toOAPIGroup(grp), Role: orch.Name}, nil
}

// LeaveGroup implements POST /api/v1/collaborate/groups/{name}/leave. It removes
// the calling agent's seat from the named group and returns the updated roster.
// The durable group record remains even when its last seat leaves. Soft
// leave-vs-terminate semantics are refined in a later stage; this stage only
// removes the seat.
func (s *Server) LeaveGroup(ctx context.Context, req oapi.LeaveGroupRequestObject) (oapi.LeaveGroupResponseObject, error) {
	if s.groups == nil {
		return nil, errStatus(http.StatusServiceUnavailable, "collaboration groups unavailable")
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return nil, errStatus(http.StatusBadRequest, "group name required")
	}
	agentRef := ""
	if req.Body != nil {
		agentRef = strings.TrimSpace(req.Body.AgentId)
	}
	if agentRef == "" {
		return nil, errStatus(http.StatusBadRequest, "agent_id required")
	}
	sess, err := s.resolveSession(ctx, agentRef)
	if err != nil {
		return nil, err
	}
	if err := s.groups.Update(name, func(g *groupstore.Group) {
		kept := g.Members[:0]
		for _, m := range g.Members {
			if m.AgentID != sess.ID {
				kept = append(kept, m)
			}
		}
		g.Members = kept
	}); err != nil {
		if errors.Is(err, groupstore.ErrNotFound) {
			return nil, errStatus(http.StatusNotFound, "group not found")
		}
		return nil, err
	}
	grp, err := s.groups.Get(name)
	if err != nil {
		return nil, err
	}
	s.recordAuditCtx(ctx, "group.leave", name, map[string]string{"agent": sess.ID})
	s.notify()
	return oapi.LeaveGroup200JSONResponse(toOAPIGroup(grp)), nil
}

// sessionRepoDir picks the directory used to resolve an agent's project key: its
// git worktree when it has one, else its working directory.
func sessionRepoDir(sess *store.Session) string {
	if sess.Worktree != "" {
		return sess.Worktree
	}
	return sess.Workdir
}

// toOAPIMember maps a stored roster seat to its wire DTO.
func toOAPIMember(m groupstore.Member) oapi.GroupMember {
	return oapi.GroupMember{
		AgentId:    m.AgentID,
		ProjectKey: m.ProjectKey,
		Summary:    m.Summary,
		JoinedAt:   m.JoinedAt,
	}
}

// toOAPIGroup maps a stored group to its wire DTO, preserving an empty (non-nil)
// roster as [] rather than null.
func toOAPIGroup(g *groupstore.Group) oapi.Group {
	members := make([]oapi.GroupMember, 0, len(g.Members))
	for _, m := range g.Members {
		members = append(members, toOAPIMember(m))
	}
	return oapi.Group{Name: g.Name, Members: members}
}
