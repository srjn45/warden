package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/daemon/oapi"
	"github.com/srjn45/warden/internal/groupstore"
	"github.com/srjn45/warden/internal/mailbox"
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
	member := groupstore.Member{
		AgentID:    sess.ID,
		ProjectKey: projectKey,
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
// The durable group record remains even when its last seat leaves.
//
// Soft-leave semantics (B6): the seat is vacated and each remaining peer receives
// a "no new inbound" notice. In-flight replies already sent to the leaving agent's
// id still route because the mailbox is addressed by agent-id, not group
// membership — this requires no extra mechanism.
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

	// Snapshot the remaining peers BEFORE removing the seat so the soft-leave
	// notice reaches everyone who shared the group with the leaving agent.
	var peers []string
	if g, gerr := s.groups.Get(name); gerr == nil {
		for _, m := range g.Members {
			if m.AgentID != sess.ID {
				peers = append(peers, m.AgentID)
			}
		}
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

	// Soft-leave notice: peers know no new inbound will come from the leaver;
	// in-flight replies (already addressed by agent-id) still route normally.
	who := sessionDisplayName(sess)
	notice := fmt.Sprintf("ℹ️ warden: %s has left group %q. "+
		"No new inbound from it; replies already in flight still deliver.", who, name)
	for _, peer := range peers {
		s.notifyGroupMember(ctx, peer, notice)
	}

	s.notify()
	return oapi.LeaveGroup200JSONResponse(toOAPIGroup(grp)), nil
}

// groupsForAgent returns every group agentID currently holds a seat in. It is the
// membership lookup behind the terminate friction gate (B6): a hard terminate of a
// grouped orchestrator orphans peers, so the terminate path must know the seats
// first. Returns nil when groups are unconfigured (the feature is simply off).
func (s *Server) groupsForAgent(agentID string) ([]*groupstore.Group, error) {
	if s.groups == nil {
		return nil, nil
	}
	all, err := s.groups.List()
	if err != nil {
		return nil, err
	}
	var out []*groupstore.Group
	for _, g := range all {
		for _, m := range g.Members {
			if m.AgentID == agentID {
				out = append(out, g)
				break
			}
		}
	}
	return out, nil
}

// terminateConfirmResponse builds the 409 that fronts the terminate friction gate:
// it names each group the target seats and the peers whose in-flight work would be
// abandoned, so the caller can weigh the cost before re-issuing with confirm=true.
func terminateConfirmResponse(sess *store.Session, groups []*groupstore.Group) oapi.TerminateSession409JSONResponse {
	who := sessionDisplayName(sess)
	seats := make([]oapi.GroupTerminateSeat, 0, len(groups))
	names := make([]string, 0, len(groups))
	for _, g := range groups {
		peers := make([]string, 0, len(g.Members))
		for _, m := range g.Members {
			if m.AgentID != sess.ID {
				peers = append(peers, m.AgentID)
			}
		}
		seats = append(seats, oapi.GroupTerminateSeat{Name: g.Name, Peers: peers})
		names = append(names, g.Name)
	}
	return oapi.TerminateSession409JSONResponse{
		Error: "terminating " + who + " abandons its seat in group(s) " +
			strings.Join(names, ", ") + " and orphans peers' in-flight work; " +
			"re-run with confirm=true to proceed",
		Groups: seats,
	}
}

// abandonGroupSeats runs the hard-terminate teardown (design §4.4): it vacates the
// terminated agent's seat in each group and notifies each remaining peer that the
// work it delegated is orphaned. Best-effort throughout — a store or mailbox
// failure is logged and swallowed so it can never fail the terminate that already
// tore the agent down. Deep per-delegation tracking is out of scope (v1 minimal):
// the group roster is the proxy for "who had outstanding work with this agent".
func (s *Server) abandonGroupSeats(ctx context.Context, sess *store.Session, groups []*groupstore.Group) {
	if s.groups == nil {
		return
	}
	who := sessionDisplayName(sess)
	for _, g := range groups {
		// Snapshot the peers before removing the seat so a notice reaches everyone
		// who shared the group with the terminated agent.
		var peers []string
		for _, m := range g.Members {
			if m.AgentID != sess.ID {
				peers = append(peers, m.AgentID)
			}
		}
		if err := s.groups.Update(g.Name, func(grp *groupstore.Group) {
			kept := grp.Members[:0]
			for _, m := range grp.Members {
				if m.AgentID != sess.ID {
					kept = append(kept, m)
				}
			}
			grp.Members = kept
		}); err != nil && !errors.Is(err, groupstore.ErrNotFound) {
			slog.Warn("group: vacate terminated seat failed", "group", g.Name, "agent", sess.ID, "err", err)
		}
		notice := fmt.Sprintf("⚠️ warden: %s (orchestrator in group %q) was terminated. "+
			"Any in-flight work you delegated to it is abandoned — it will not reply. "+
			"Re-delegate elsewhere if the work still matters.", who, g.Name)
		for _, peer := range peers {
			s.notifyGroupMember(ctx, peer, notice)
		}
		s.recordAuditCtx(ctx, "group.abandon", g.Name, map[string]string{
			"agent": sess.ID,
			"peers": strconv.Itoa(len(peers)),
		})
	}
}

// notifyGroupMember lands a warden-originated notice in one member's inbox and
// nudges its pane if parked, mirroring WakePipelineOwner's trusted daemon-internal
// write (reserved "daemon" sender, no sanitizeSender gate). Best-effort: a missing
// mailbox or absent recipient is a silent no-op.
func (s *Server) notifyGroupMember(ctx context.Context, memberID, body string) {
	if s.mbox == nil || memberID == "" {
		return
	}
	if _, err := s.mbox.Append(mailbox.Message{To: memberID, From: pipelineWakeSender, Body: body}); err != nil {
		slog.Warn("group: deliver abandonment notice failed", "member", memberID, "err", err)
		return
	}
	if sess, err := s.store.Get(ctx, memberID); err == nil && sess != nil && parked(sess.Status) {
		_ = s.life.Input(ctx, sess.TmuxSession, groupNotifyNudge)
	}
	s.notify()
}

// groupNotifyNudge is the pane prod injected into a parked member so it surfaces a
// warden group notice (the durable content is already in the inbox; this only
// prompts the agent to read it), mirroring pipelineWakeNotice.
const groupNotifyNudge = "📨 warden: a collaboration-group update arrived. Run `warden msg inbox` to read."

// sessionDisplayName is the human-facing label for an agent in notices/gates: its
// friendly name when set, else its id.
func sessionDisplayName(sess *store.Session) string {
	if sess.Name != "" {
		return sess.Name
	}
	return sess.ID
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
