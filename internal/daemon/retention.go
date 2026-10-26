package daemon

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"time"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/audit"
)

// retentionSweepInterval is how often expired archived agents are purged. An
// agent archived with a 24h window is therefore gone between 24h and 25h later.
const retentionSweepInterval = time.Hour

// mailboxPeers are inbox owners that are not agents and are never orphaned.
var mailboxPeers = map[string]bool{"human": true, "daemon": true, "system": true}

// purger is the optional store capability the retention sweep needs.
type purger interface {
	Purge(ctx context.Context, id string) error
}

// SetRetention wires the data-retention policy
// (docs/specs/2026-10-26-data-retention.md). archiveKeep is how long an
// archived agent stays restorable (0 keeps archived agents forever); readKeep
// is how long a delivered message is kept (0 deletes it on delivery);
// metricsDays bounds metrics history.
func (s *Server) SetRetention(archiveKeep, readKeep time.Duration, metricsDays int) {
	s.archiveKeep = archiveKeep
	s.metricsKeepDays = metricsDays
	if s.mbox != nil {
		s.mbox.SetReadRetention(readKeep)
	}
}

func (s *Server) metricsRetention() int {
	if s.metricsKeepDays > 0 {
		return s.metricsKeepDays
	}
	return metricsRetentionDays
}

// runRetentionSweep purges expired archived agents at startup and then hourly.
func (s *Server) runRetentionSweep(ctx context.Context) {
	s.retentionSweep(ctx, time.Now())
	t := time.NewTicker(retentionSweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.done:
			return
		case <-t.C:
			s.retentionSweep(ctx, time.Now())
		}
	}
}

// archivedAt is when a was archived. Records archived before archived_at
// existed fall back to their last update.
func archivedAt(a *agentstore.Agent) time.Time {
	if a.ArchivedAt != nil {
		return *a.ArchivedAt
	}
	return a.UpdatedAt
}

// retentionSweep purges every archived agent whose restore window has passed,
// with its inbox, then deletes inboxes whose recipient no longer exists. An
// archived parent that is still referenced is kept and retried next sweep;
// children are purged first so parents follow once their subtree is gone.
// It returns how many agents it purged. Best-effort: errors are logged.
func (s *Server) retentionSweep(ctx context.Context, now time.Time) int {
	if s.archiveKeep <= 0 || s.store == nil {
		return 0
	}
	p, ok := s.store.(purger)
	if !ok {
		return 0
	}
	closed, err := s.store.ListClosed(ctx)
	if err != nil {
		slog.Warn("retention: list archived agents", "err", err)
		return 0
	}
	cutoff := now.Add(-s.archiveKeep)
	expired := map[string]*agentstore.Agent{}
	for _, a := range closed {
		if archivedAt(a).Before(cutoff) {
			expired[a.ID] = a
		}
	}
	purged := 0
	// Repeat so a parent whose expired children were purged this pass follows
	// them in the same sweep.
	for progress := true; progress && len(expired) > 0; {
		progress = false
		for id := range expired {
			if ctx.Err() != nil {
				return purged
			}
			err := p.Purge(ctx, id)
			switch {
			case err == nil, errors.Is(err, agentstore.ErrNotFound):
				delete(expired, id)
				if s.mbox != nil {
					_ = s.mbox.DeleteInbox(id)
				}
				if err == nil {
					purged++
				}
				progress = true
			case errors.Is(err, agentstore.ErrStillReferenced), errors.Is(err, agentstore.ErrNotArchived):
			default:
				slog.Warn("retention: purge archived agent", "id", id, "err", err)
				delete(expired, id)
			}
		}
	}
	kept := len(expired)
	orphans := s.dropOrphanInboxes(ctx)
	if purged > 0 || orphans > 0 {
		slog.Info("retention: sweep", "purged_agents", purged, "kept_referenced", kept, "orphan_inboxes", orphans)
		if s.audit != nil {
			s.audit.Log(audit.Event{Action: audit.ActionRetentionSweep, Actor: "daemon", Detail: map[string]string{
				"purged_agents":   strconv.Itoa(purged),
				"kept_referenced": strconv.Itoa(kept),
				"orphan_inboxes":  strconv.Itoa(orphans),
				"archive_keep":    s.archiveKeep.String(),
			}})
		}
		s.notify()
	}
	return purged
}

// dropOrphanInboxes deletes inboxes whose recipient is neither a live nor an
// archived agent: nothing can ever read them.
func (s *Server) dropOrphanInboxes(ctx context.Context) int {
	if s.mbox == nil {
		return 0
	}
	msgs, err := s.mbox.All()
	if err != nil {
		return 0
	}
	known := map[string]bool{}
	live, err := s.store.List(ctx)
	if err != nil {
		return 0
	}
	closed, err := s.store.ListClosed(ctx)
	if err != nil {
		return 0
	}
	for _, a := range append(live, closed...) {
		known[a.ID] = true
	}
	n := 0
	seen := map[string]bool{}
	for _, m := range msgs {
		if seen[m.To] || known[m.To] || mailboxPeers[m.To] || m.To == "" {
			continue
		}
		seen[m.To] = true
		// Terminals and other non-agent sessions also receive messages.
		if _, err := s.lookupTerminal(ctx, m.To); err == nil {
			continue
		}
		if s.mbox.DeleteInbox(m.To) == nil {
			n++
		}
	}
	return n
}
