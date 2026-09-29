package backendstore

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/srjn45/scriva/engine"
)

// RLCooldownEntry records a confirmed-hard-limit cooldown for one backend/model
// pair. Stored in the "rl_cooldowns" ScrivaDB collection, keyed by
// backendID + NUL + modelID. This collection is never touched by SyncToStore
// or any quota-window machinery; it holds only reactive recovery evidence.
type RLCooldownEntry struct {
	BackendID    string    `json:"backend_id"`
	ModelID      string    `json:"model_id"`
	LimitedUntil time.Time `json:"limited_until"`
	RecordedAt   time.Time `json:"recorded_at"`
}

// rlCooldownKey returns the ScrivaDB key for a (backendID, modelID) pair.
// The NUL separator is chosen because valid backend IDs and model IDs never
// contain NUL; it guarantees a collision-free composite key.
func rlCooldownKey(backendID, modelID string) string {
	return backendID + "\x00" + modelID
}

func rlCooldownFromRecord(d map[string]any) (RLCooldownEntry, error) {
	b, err := json.Marshal(d)
	if err != nil {
		return RLCooldownEntry{}, err
	}
	var out RLCooldownEntry
	if err := json.Unmarshal(b, &out); err != nil {
		return RLCooldownEntry{}, err
	}
	return out, nil
}

// SetRLCooldown records (or updates) a confirmed-hard-limit cooldown for
// (backendID, modelID) expiring at `until`. A zero or past `until` clears any
// existing record; the candidate becomes eligible again on the next advance().
// This is the sole write path for reactive recovery cooldown evidence; it is
// distinct from quota window tracking and is never overwritten by SyncToStore.
func (s *Store) SetRLCooldown(backendID, modelID string, until time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if backendID == "" || modelID == "" {
		return nil
	}
	key := rlCooldownKey(backendID, modelID)
	if !until.After(time.Now().UTC()) {
		// Expired or zero: remove any existing record; never store a stale one.
		_ = s.rlCooldownsCol.DeleteByKey(key)
		return nil
	}
	entry := RLCooldownEntry{
		BackendID:    backendID,
		ModelID:      modelID,
		LimitedUntil: until.UTC(),
		RecordedAt:   time.Now().UTC(),
	}
	rec, err := toRecord(entry)
	if err != nil {
		return err
	}
	_, getErr := s.rlCooldownsCol.GetByKey(key)
	if errors.Is(getErr, engine.ErrKeyNotFound) {
		_, _, err = s.rlCooldownsCol.InsertWithKey(key, rec)
		return err
	}
	if getErr != nil {
		return getErr
	}
	_, err = s.rlCooldownsCol.UpdateByKey(key, rec)
	return err
}

// IsRLCoolingDown reports whether the confirmed-hard-limit cooldown for
// (backendID, modelID) is still active at `now`. A zero `now` uses the
// current wall clock. Returns false on any read/parse error (fail-open so a
// corrupt record never permanently blocks a candidate).
func (s *Store) IsRLCoolingDown(backendID, modelID string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if backendID == "" || modelID == "" {
		return false
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	key := rlCooldownKey(backendID, modelID)
	r, err := s.rlCooldownsCol.GetByKey(key)
	if errors.Is(err, engine.ErrKeyNotFound) {
		return false
	}
	if err != nil {
		return false
	}
	entry, err := rlCooldownFromRecord(r.Data)
	if err != nil {
		return false
	}
	if !entry.LimitedUntil.After(now) {
		// Expired: delete so the collection does not accumulate stale evidence.
		// Fail-open on delete errors — a leftover expired row still reads as
		// not cooling down on the next check.
		_ = s.rlCooldownsCol.DeleteByKey(key)
		return false
	}
	return true
}
