package daemon

import (
	"context"
	"time"

	"github.com/srjn45/warden/internal/groupstore"
	"github.com/srjn45/warden/internal/store"
)

// RecoverResult is one archived record's recovery candidacy/outcome.
type RecoverResult struct {
	ID          string
	TmuxSession string
	Workdir     string
	Name        string
	Subject     string
	ParentID    string
	Recovered   bool
	Error       string
}

// Recover scans archived (closed) records for ones whose tmux session is
// confirmed still alive. This is the safety net for the tombstone reaper
// (tombstone_reap.go): a stale orphaned status racing a daemon restart could
// previously get a live session's record archived out from under it (see the
// liveness reconfirm added there), and this is how to bring one back if it
// ever happens regardless.
//
// apply=false only reports candidates and changes nothing. apply=true
// re-inserts each candidate's full original record into the active store
// under its original id, so any children (linked via ParentID, a one-way
// pointer never touched by archiving) reconnect automatically with no edits
// of their own. The stale closed copy is left in place — recovering does not
// remove archived history.
//
// When apply=true, recovered orchestrators are also automatically re-seated in
// their collaboration groups and their peers are notified (B7: recover
// auto-rejoin).
func (s *Server) Recover(ctx context.Context, apply bool) ([]RecoverResult, error) {
	var alive func(ctx context.Context, tmuxSession string) bool
	if s.poller != nil {
		alive = s.poller.SessionAlive
	}
	results, err := recoverCandidates(ctx, s.store, alive, apply)
	if err != nil {
		return nil, err
	}
	if apply {
		var recovered []*store.Session
		for _, r := range results {
			if !r.Recovered {
				continue
			}
			if sess, serr := s.store.Get(ctx, r.ID); serr == nil {
				recovered = append(recovered, sess)
			}
		}
		if len(recovered) > 0 {
			s.rejoinGroups(ctx, recovered)
		}
	}
	return results, nil
}

// rejoinGroups re-seats recovered orchestrators in their collaboration groups
// and re-announces their return to peers (B7: recover auto-rejoin). The group
// record is durable so membership entries already exist; this refreshes the
// JoinedAt timestamp (so peers can see when the agent came back) and delivers a
// targeted re-announce notice to each peer. Best-effort throughout — failures
// are logged but never returned (recovery itself must not be interrupted).
func (s *Server) rejoinGroups(ctx context.Context, recovered []*store.Session) {
	if s.groups == nil {
		return
	}
	now := time.Now().UTC()
	for _, sess := range recovered {
		groups, err := s.groups.GroupsForAgent(sess.ID)
		if err != nil || len(groups) == 0 {
			continue
		}
		for _, grp := range groups {
			// Refresh the JoinedAt so peers can see the recovery time.
			var mem groupstore.Member
			_ = s.groups.Update(grp.Name, func(g *groupstore.Group) {
				for i := range g.Members {
					if g.Members[i].AgentID == sess.ID {
						g.Members[i].JoinedAt = now
						mem = g.Members[i]
						break
					}
				}
			})
			if mem.AgentID == "" {
				continue
			}
			// Re-read so the updated record is used for the announcement.
			fresh, ferr := s.groups.Get(grp.Name)
			if ferr != nil {
				continue
			}
			s.brokerReannounce(ctx, fresh, mem, sess.Name)
		}
	}
}

// recoverCandidates is Recover's testable core: the store and liveness check
// are passed in directly rather than read off *Server, so tests can exercise
// it with a fake store and a controllable alive func with no real poller.
func recoverCandidates(ctx context.Context, st store.Store, alive func(ctx context.Context, tmuxSession string) bool, apply bool) ([]RecoverResult, error) {
	closed, err := st.ListClosed(ctx)
	if err != nil {
		return nil, err
	}
	var results []RecoverResult
	for _, rec := range closed {
		if rec.TmuxSession == "" || alive == nil || !alive(ctx, rec.TmuxSession) {
			continue
		}
		// Already reinstated (e.g. a previous recover run, or it was never
		// really gone) — not a candidate.
		if _, err := st.Get(ctx, rec.ID); err == nil {
			continue
		}
		res := RecoverResult{
			ID: rec.ID, TmuxSession: rec.TmuxSession, Workdir: rec.Workdir,
			Name: rec.Name, Subject: rec.Subject, ParentID: rec.ParentID,
		}
		if apply {
			revived := *rec
			revived.Status = store.StatusWorking
			revived.PID = 0
			revived.ExitCode = nil
			revived.UpdatedAt = time.Now()
			if err := st.Insert(ctx, &revived); err != nil {
				res.Error = err.Error()
			} else {
				res.Recovered = true
			}
		}
		results = append(results, res)
	}
	return results, nil
}
