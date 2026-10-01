package plansync

import (
	"time"

	"github.com/srjn45/warden/internal/planstore"
)

// StampPlan writes Hub sync metadata onto a canonical Plan. Call only after a
// successful Hub Push/Pull that produced a non-empty remoteID. Local/Fake
// providers and planexport / sync_to_repo must never invoke this.
func StampPlan(p *planstore.Plan, remoteID string, syncedAt time.Time) {
	if p == nil || remoteID == "" {
		return
	}
	t := syncedAt.UTC()
	p.SyncedAt = &t
	p.RemoteID = remoteID
}

// StampPlanFromEnvelope copies Hub-assigned SyncedAt / RemoteID from env onto p.
// No-op when env.RemoteID is empty (Local/Fake envelopes stay unstamped).
func StampPlanFromEnvelope(p *planstore.Plan, env Envelope) {
	if env.RemoteID == "" || env.SyncedAt == nil {
		return
	}
	StampPlan(p, env.RemoteID, *env.SyncedAt)
}
