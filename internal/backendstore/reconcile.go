package backendstore

import (
	"errors"
	"time"

	"github.com/srjn45/warden/internal/agentbackend"
)

// Reconcile folds a detection sweep into the store, preserving the user's
// preferences (docs/specs/2026-08-06-backend-registry.md §4). It writes ONLY the
// detection fields — Installed / BinaryPath / DetectedAt — and never touches
// Tier / Default / Enabled, so those survive an uninstall/reinstall. A backend
// seen for the first time starts unclassified + enabled.
//
// The reserved local row no longer exists (local LLM was dropped); any row a
// previous daemon persisted is pruned here, like terminal.
//
// terminal was a backend before the cockpit stage-6 redesign; it is no longer
// registered (terminals are the session Kind=terminal now, created via the spawn
// `kind` field), so Detect never returns it. Any `terminal` row a pre-stage-6
// daemon persisted is pruned here so it stops appearing in GET /api/v1/backends —
// a one-time, idempotent migration folded into every reconcile.
func Reconcile(store *Store, det []agentbackend.Detected, now time.Time) error {
	for _, d := range det {
		b, err := store.Get(d.ID)
		if errors.Is(err, ErrNotFound) {
			b = Backend{ID: d.ID, Tier: TierUnclassified, Enabled: true} // first sight
		} else if err != nil {
			return err
		}
		b.Installed = d.Installed
		b.BinaryPath = d.Path
		b.DetectedAt = now
		if err := store.Upsert(b); err != nil {
			return err
		}
	}
	if err := store.Delete(idTerminal); err != nil { // prune a stale pre-stage-6 terminal row
		return err
	}
	return store.Delete(idLocal) // prune any pre-Fast-Brain reserved local row
}
