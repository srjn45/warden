package daemon

import (
	"context"
	"time"

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
func (s *Server) Recover(ctx context.Context, apply bool) ([]RecoverResult, error) {
	var alive func(ctx context.Context, tmuxSession string) bool
	if s.poller != nil {
		alive = s.poller.SessionAlive
	}
	return recoverCandidates(ctx, s.store, alive, apply)
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
