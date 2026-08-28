// Package backendstore persists the agent-backend registry (docs/specs/
// 2026-08-06-backend-registry.md): one record per detected backend plus a single
// reserved settings record, all in an embedded ScrivaDB "backends" collection.
//
// The DB is the single source of truth for which backends exist, their tier, and
// which is the default. Detection is a fact (Installed/BinaryPath/DetectedAt);
// tiering is a preference (Tier/Default/Enabled) — a rescan reconciles the former
// and never touches the latter. See Reconcile in internal/agentbackend.
package backendstore

import (
	"encoding/json"
	"errors"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/srjn45/scriva"
	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/scriva/query"
)

var (
	// ErrNotFound is the store-boundary translation of engine.ErrKeyNotFound.
	ErrNotFound = errors.New("backend not found")
	// ErrExists is the store-boundary translation of engine.ErrDuplicateKey.
	ErrExists = errors.New("backend already exists")
	// ErrModelNotFound is returned when a model is not found in the catalog.
	ErrModelNotFound = errors.New("model not found")
	// ErrRoleNotFound is returned when a role tier mapping is not found.
	ErrRoleNotFound = errors.New("role tier mapping not found")
	// ErrInvalidTier is returned when an invalid model tier is specified.
	ErrInvalidTier = errors.New("invalid model tier")
)

const (
	// SettingsKey is the reserved ScrivaDB key of the singleton Settings record.
	// It shares the "backends" collection but is excluded from List() so it never
	// surfaces as a backend row.
	SettingsKey = "__settings__"

	// HandoverSettingsKey is the reserved ScrivaDB key of the singleton HandoverSettings record.
	HandoverSettingsKey = "__handover_settings__"

	// ThinkingModeLocalOnly routes internal thinking to the local model only.
	ThinkingModeLocalOnly = "local_only"
	// ThinkingModeFreePlusLocal prefers free cloud backends and falls back to the
	// never-limited local model. This is the default.
	ThinkingModeFreePlusLocal = "free_plus_local"

	// TierLocal is the reserved, system-set tier of the local-model row: a $0,
	// never-limited class. It is not user-assignable.
	TierLocal = "local"
	// TierUnclassified is the tier a newly detected CLI backend starts in until
	// the user tiers it — treated as *not free*.
	TierUnclassified = "unclassified"
	// TierFree is the $0 tier: a CLI backend the user runs on a free plan, the
	// only CLI tier the internal-thinking router (docs/specs/
	// 2026-08-06-backend-registry.md §7) will call. TierSubscription and
	// TierPayPerUse are the paid tiers the router NEVER calls.
	TierFree         = "free"
	TierSubscription = "subscription"
	TierPayPerUse    = "pay_per_use"

	// idLocal is the reserved id of the local-model row; idTerminal is the host
	// shell. Neither can be a user-agent default.
	idLocal    = "local"
	idTerminal = "terminal"

	// IDLocal is the exported id of the reserved local-model row — the terminal
	// candidate of every internal-thinking walk (§7). Exported so the router can
	// name the local candidate without duplicating the literal.
	IDLocal = idLocal
)

// Backend is one row of the registry, keyed by its stable ID in ScrivaDB.
type Backend struct {
	ID           string    `json:"id"`          // "claude", "local", … — stable ScrivaDB key
	Installed    bool      `json:"installed"`   // Binary() on PATH (or local endpoint reachable)
	BinaryPath   string    `json:"binary_path"` // resolved LookPath (empty for local)
	DetectedAt   time.Time `json:"detected_at"`
	Tier         string    `json:"tier"`    // free|subscription|pay_per_use|unclassified|local
	Default      bool      `json:"default"` // exactly one row may be true
	Enabled      bool      `json:"enabled"`
	IsLocal      bool      `json:"is_local"`               // the local-model row (never limited, never a user default)
	LimitedUntil time.Time `json:"limited_until,omitzero"` // always zero when IsLocal
}

// Settings is a singleton record (reserved key SettingsKey in the same
// collection) holding store-level policy that is not per-backend.
type Settings struct {
	ID                   string `json:"id"`                     // SettingsKey
	InternalThinkingMode string `json:"internal_thinking_mode"` // local_only | free_plus_local
	AllowPaidAutopilot   bool   `json:"allow_paid_autopilot"`   // migrated from autopilot.allow_pay_per_use
}

// Store persists the registry as records in an embedded ScrivaDB "backends"
// collection, one record per backend keyed by its ID plus the reserved
// SettingsKey record. Opened SyncModeNone: this is a localhost daemon store, so
// last-write-survives-power-loss is not required (append-only segments rule out
// torn reads regardless).
//
// A single mutex serialises the compound read-modify-write methods (SetTier /
// SetEnabled / SetDefault / SetThinkingMode) and the exists-check-then-write in
// Upsert; ScrivaDB does its own per-collection locking, so the store mutex only
// guards the read-then-write critical sections. Read-only methods take it too for
// a behaviour-identical mutex model.
type Store struct {
	mu          sync.Mutex
	db          *scriva.DB
	col         *engine.Collection
	modelsCol   *engine.Collection
	rolesCol    *engine.Collection
	handoverCol *engine.Collection
	quotasCol   *engine.Collection
}

// NewStore opens (creating if needed) the ScrivaDB-backed backend registry at
// dir (~/.warden/backends). This is a fresh store: there is no legacy JSON to
// import, so none of the schedule store's sentinel/import machinery applies.
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	db, err := scriva.Open(dir, scriva.WithSyncMode(engine.SyncModeNone))
	if err != nil {
		return nil, err
	}
	col, err := db.Collection("backends")
	if err != nil {
		db.Close()
		return nil, err
	}
	modelsCol, err := db.Collection("models")
	if err != nil {
		db.Close()
		return nil, err
	}
	rolesCol, err := db.Collection("role_tiers")
	if err != nil {
		db.Close()
		return nil, err
	}
	handoverCol, err := db.Collection("handover_settings")
	if err != nil {
		db.Close()
		return nil, err
	}
	quotasCol, err := db.Collection("quotas")
	if err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{
		db:          db,
		col:         col,
		modelsCol:   modelsCol,
		rolesCol:    rolesCol,
		handoverCol: handoverCol,
		quotasCol:   quotasCol,
	}
	if err := s.seedDefaultsIfEmpty(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// toRecord decomposes v into a ScrivaDB record body via a JSON round-trip, so its
// fields stay real in the store rather than an opaque blob. The engine stamps the
// reserved key field on InsertWithKey/UpdateByKey, so it must NOT be present here.
func toRecord(v any) (map[string]any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// backendFromRecord reconstructs a Backend from a record body. The reserved key
// field the engine stamped into the map is harmlessly dropped on unmarshal
// (Backend has no matching json tag).
func backendFromRecord(d map[string]any) (Backend, error) {
	b, err := json.Marshal(d)
	if err != nil {
		return Backend{}, err
	}
	var out Backend
	if err := json.Unmarshal(b, &out); err != nil {
		return Backend{}, err
	}
	return out, nil
}

// List returns every backend row sorted by ID. The reserved SettingsKey record is
// filtered out so it never appears as a backend. An undecodable record is skipped
// rather than failing the whole scan.
func (s *Store) List() ([]Backend, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.list()
}

// list is the unlocked body of List; callers hold s.mu.
func (s *Store) list() ([]Backend, error) {
	results, err := s.col.Scan(query.MatchAll)
	if err != nil {
		return nil, err
	}
	out := make([]Backend, 0, len(results))
	for _, r := range results {
		if idOf(r.Data) == SettingsKey {
			continue
		}
		b, err := backendFromRecord(r.Data)
		if err != nil {
			continue
		}
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// idOf reads the "id" field of a record body, tolerating a missing/typed value.
func idOf(d map[string]any) string {
	if v, ok := d["id"].(string); ok {
		return v
	}
	return ""
}

// Get returns one backend by id, or ErrNotFound.
func (s *Store) Get(id string) (Backend, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.get(id)
}

// get reads the backend keyed id, mapping a key miss to ErrNotFound. The caller
// holds s.mu.
func (s *Store) get(id string) (Backend, error) {
	r, err := s.col.GetByKey(id)
	if errors.Is(err, engine.ErrKeyNotFound) {
		return Backend{}, ErrNotFound
	}
	if err != nil {
		return Backend{}, err
	}
	return backendFromRecord(r.Data)
}

// Upsert inserts b or updates it in place, keyed by b.ID.
func (s *Store) Upsert(b Backend) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.upsert(b)
}

// upsert is the unlocked body of Upsert; callers hold s.mu.
func (s *Store) upsert(b Backend) error {
	rec, err := toRecord(b)
	if err != nil {
		return err
	}
	_, err = s.col.GetByKey(b.ID)
	if errors.Is(err, engine.ErrKeyNotFound) {
		if _, _, err := s.col.InsertWithKey(b.ID, rec); err != nil {
			if errors.Is(err, engine.ErrDuplicateKey) {
				return ErrExists
			}
			return err
		}
		return nil
	}
	if err != nil {
		return err
	}
	_, err = s.col.UpdateByKey(b.ID, rec)
	return err
}

// Delete removes the backend row keyed id. A missing row is not an error
// (idempotent) — callers use it to prune a row that should no longer exist, e.g.
// a `terminal` row a pre-stage-6 daemon persisted before the backend was removed.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.col.DeleteByKey(id); err != nil && !errors.Is(err, engine.ErrKeyNotFound) {
		return err
	}
	return nil
}

// SetTier records the user's tier preference for a backend (RMW). It does not
// validate the tier string — the API layer owns that (§6); the reserved "local"
// tier is system-set, not routed through here.
func (s *Store) SetTier(id, tier string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := s.get(id)
	if err != nil {
		return err
	}
	b.Tier = tier
	return s.upsert(b)
}

// SetEnabled toggles a backend's Enabled flag (RMW).
func (s *Store) SetEnabled(id string, on bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := s.get(id)
	if err != nil {
		return err
	}
	b.Enabled = on
	return s.upsert(b)
}

// SetLimited stamps a backend's LimitedUntil (RMW): the internal-thinking router
// (docs/specs/2026-08-06-backend-registry.md §7) calls this when a free CLI
// candidate returns a rate-limit / spend signal, so the row drops out of the
// candidate walk until until elapses. The reserved local row can never be
// limited, so this is a no-op for it (defensive — the router never stamps local).
func (s *Store) SetLimited(id string, until time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := s.get(id)
	if err != nil {
		return err
	}
	if b.IsLocal {
		return nil // local can never be limited
	}
	b.LimitedUntil = until
	return s.upsert(b)
}

// SetDefault makes id the single default backend: it clears every other row's
// Default flag and sets this one, atomically under s.mu (the single-default
// invariant). The reserved local and terminal ids can never be a user-agent
// default and are rejected. Returns ErrNotFound if id is absent.
func (s *Store) SetDefault(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == idLocal || id == idTerminal {
		return errors.New("backend " + id + " cannot be the default")
	}
	target, err := s.get(id)
	if err != nil {
		return err
	}
	all, err := s.list()
	if err != nil {
		return err
	}
	// Clear any other current default first.
	for _, b := range all {
		if b.ID != id && b.Default {
			b.Default = false
			if err := s.upsert(b); err != nil {
				return err
			}
		}
	}
	target.Default = true
	return s.upsert(target)
}

// Default returns the flagged default backend and true, or a zero Backend and
// false when no row is default.
func (s *Store) Default() (Backend, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.list()
	if err != nil {
		return Backend{}, false, err
	}
	for _, b := range all {
		if b.Default {
			return b, true, nil
		}
	}
	return Backend{}, false, nil
}

// Settings returns the singleton settings record with defaults applied (a missing
// record yields the default free_plus_local mode).
func (s *Store) Settings() (Settings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.settings()
}

// settings is the unlocked body of Settings; callers hold s.mu.
func (s *Store) settings() (Settings, error) {
	out := Settings{ID: SettingsKey, InternalThinkingMode: ThinkingModeFreePlusLocal}
	r, err := s.col.GetByKey(SettingsKey)
	if errors.Is(err, engine.ErrKeyNotFound) {
		return out, nil
	}
	if err != nil {
		return Settings{}, err
	}
	b, err := json.Marshal(r.Data)
	if err != nil {
		return Settings{}, err
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return Settings{}, err
	}
	if out.InternalThinkingMode == "" {
		out.InternalThinkingMode = ThinkingModeFreePlusLocal
	}
	out.ID = SettingsKey
	return out, nil
}

// SetThinkingMode persists the internal-thinking mode (RMW on the settings
// record). It rejects any mode other than local_only / free_plus_local.
func (s *Store) SetThinkingMode(mode string) error {
	if mode != ThinkingModeLocalOnly && mode != ThinkingModeFreePlusLocal {
		return errors.New("invalid internal-thinking mode " + mode)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, err := s.settings()
	if err != nil {
		return err
	}
	cur.InternalThinkingMode = mode
	return s.putSettings(cur)
}

// SetAllowPaidAutopilot persists the paid-autopilot gate (RMW on the settings
// record). It is the store-side home of the deprecated autopilot.brain.
// allow_pay_per_use config key (docs/specs/2026-08-06-backend-registry.md §8):
// the one-time ladder migration seeds it, and thereafter the store value is
// authoritative for whether autopilot's cost-tier selection may reach a
// pay_per_use backend.
func (s *Store) SetAllowPaidAutopilot(on bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, err := s.settings()
	if err != nil {
		return err
	}
	cur.AllowPaidAutopilot = on
	return s.putSettings(cur)
}

// AutopilotLadder derives autopilot's cost-tier backend ladder from the registry
// (docs/specs/2026-08-06-backend-registry.md §8): the store is the source of truth
// after the ladder migration, replacing the deprecated autopilot.brain.backends
// config. Only installed, enabled, non-local rows are eligible (an uninstalled or
// disabled backend is never selectable); each is placed in its tier bucket. Within
// a tier the order is List()'s stable id-ascending order, so selection is
// deterministic. allowPaid is Settings.AllowPaidAutopilot — the gate the selection
// loop applies to the pay_per_use tier.
func (s *Store) AutopilotLadder() (free, subscription, payPerUse []string, allowPaid bool, err error) {
	backends, err := s.List()
	if err != nil {
		return nil, nil, nil, false, err
	}
	st, err := s.Settings()
	if err != nil {
		return nil, nil, nil, false, err
	}
	for _, b := range backends {
		if !b.Installed || !b.Enabled || b.IsLocal {
			continue
		}
		switch b.Tier {
		case TierFree:
			free = append(free, b.ID)
		case TierSubscription:
			subscription = append(subscription, b.ID)
		case TierPayPerUse:
			payPerUse = append(payPerUse, b.ID)
		}
	}
	return free, subscription, payPerUse, st.AllowPaidAutopilot, nil
}

// putSettings inserts or updates the singleton settings record. Callers hold
// s.mu.
func (s *Store) putSettings(st Settings) error {
	st.ID = SettingsKey
	rec, err := toRecord(st)
	if err != nil {
		return err
	}
	_, err = s.col.GetByKey(SettingsKey)
	if errors.Is(err, engine.ErrKeyNotFound) {
		_, _, err = s.col.InsertWithKey(SettingsKey, rec)
		return err
	}
	if err != nil {
		return err
	}
	_, err = s.col.UpdateByKey(SettingsKey, rec)
	return err
}

// --- Models Catalog ---------------------------------------------------------

func modelKey(backendID, modelID string) string {
	return backendID + ":" + modelID
}

func modelFromRecord(d map[string]any) (ModelEntry, error) {
	b, err := json.Marshal(d)
	if err != nil {
		return ModelEntry{}, err
	}
	var out ModelEntry
	if err := json.Unmarshal(b, &out); err != nil {
		return ModelEntry{}, err
	}
	return out, nil
}

// ListModels returns all registered models, optionally filtered by tier (if tierFilter is non-empty).
// If tierFilter is non-empty and not a valid tier, ErrInvalidTier is returned.
func (s *Store) ListModels(tierFilter ModelTier) ([]ModelEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listModels(tierFilter)
}

func (s *Store) listModels(tierFilter ModelTier) ([]ModelEntry, error) {
	if tierFilter != "" && !tierFilter.Valid() {
		return nil, ErrInvalidTier
	}
	results, err := s.modelsCol.Scan(query.MatchAll)
	if err != nil {
		return nil, err
	}
	out := make([]ModelEntry, 0, len(results))
	for _, r := range results {
		m, err := modelFromRecord(r.Data)
		if err != nil {
			continue
		}
		if tierFilter != "" && m.Tier != tierFilter {
			continue
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].BackendID == out[j].BackendID {
			return out[i].ModelID < out[j].ModelID
		}
		return out[i].BackendID < out[j].BackendID
	})
	return out, nil
}

// GetModel returns the model entry for backendID and modelID, or ErrModelNotFound.
func (s *Store) GetModel(backendID, modelID string) (ModelEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getModel(backendID, modelID)
}

func (s *Store) getModel(backendID, modelID string) (ModelEntry, error) {
	if backendID == "" || modelID == "" {
		return ModelEntry{}, ErrModelNotFound
	}
	r, err := s.modelsCol.GetByKey(modelKey(backendID, modelID))
	if errors.Is(err, engine.ErrKeyNotFound) {
		return ModelEntry{}, ErrModelNotFound
	}
	if err != nil {
		return ModelEntry{}, err
	}
	return modelFromRecord(r.Data)
}

// SetModelTier updates the tier for a specific model (RMW). Returns ErrModelNotFound if missing, or ErrInvalidTier if tier is invalid.
func (s *Store) SetModelTier(backendID, modelID string, tier ModelTier) error {
	if !tier.Valid() {
		return ErrInvalidTier
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.getModel(backendID, modelID)
	if err != nil {
		return err
	}
	m.Tier = tier
	return s.upsertModel(m)
}

// SetModelEnabled enables or disables a specific model (RMW). Returns ErrModelNotFound if missing.
func (s *Store) SetModelEnabled(backendID, modelID string, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.getModel(backendID, modelID)
	if err != nil {
		return err
	}
	m.Enabled = enabled
	return s.upsertModel(m)
}

// UpsertModel inserts or updates a model entry in the store.
func (s *Store) UpsertModel(m ModelEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.upsertModel(m)
}

func (s *Store) upsertModel(m ModelEntry) error {
	if m.BackendID == "" || m.ModelID == "" {
		return errors.New("backend ID and model ID cannot be empty")
	}
	if !m.Tier.Valid() {
		return ErrInvalidTier
	}
	key := modelKey(m.BackendID, m.ModelID)
	rec, err := toRecord(m)
	if err != nil {
		return err
	}
	_, err = s.modelsCol.GetByKey(key)
	if errors.Is(err, engine.ErrKeyNotFound) {
		if _, _, err := s.modelsCol.InsertWithKey(key, rec); err != nil {
			if errors.Is(err, engine.ErrDuplicateKey) {
				return ErrExists
			}
			return err
		}
		return nil
	}
	if err != nil {
		return err
	}
	_, err = s.modelsCol.UpdateByKey(key, rec)
	return err
}

// --- Role Tier Mappings -----------------------------------------------------

func roleTierFromRecord(d map[string]any) (RoleTierMapping, error) {
	b, err := json.Marshal(d)
	if err != nil {
		return RoleTierMapping{}, err
	}
	var out RoleTierMapping
	if err := json.Unmarshal(b, &out); err != nil {
		return RoleTierMapping{}, err
	}
	return out, nil
}

// ListRoleTiers returns all role-to-tier mappings sorted by role name.
func (s *Store) ListRoleTiers() ([]RoleTierMapping, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listRoleTiers()
}

func (s *Store) listRoleTiers() ([]RoleTierMapping, error) {
	results, err := s.rolesCol.Scan(query.MatchAll)
	if err != nil {
		return nil, err
	}
	out := make([]RoleTierMapping, 0, len(results))
	for _, r := range results {
		rm, err := roleTierFromRecord(r.Data)
		if err != nil {
			continue
		}
		out = append(out, rm)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].RoleName < out[j].RoleName
	})
	return out, nil
}

// GetRoleTier returns the default model tier for a role, or ErrRoleNotFound.
func (s *Store) GetRoleTier(roleName string) (ModelTier, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getRoleTier(roleName)
}

func (s *Store) getRoleTier(roleName string) (ModelTier, error) {
	if roleName == "" {
		return "", ErrRoleNotFound
	}
	r, err := s.rolesCol.GetByKey(roleName)
	if errors.Is(err, engine.ErrKeyNotFound) {
		return "", ErrRoleNotFound
	}
	if err != nil {
		return "", err
	}
	rm, err := roleTierFromRecord(r.Data)
	if err != nil {
		return "", err
	}
	return rm.DefaultTier, nil
}

// SetRoleTier sets or updates the model tier mapping for a role. Returns ErrInvalidTier if tier is invalid.
func (s *Store) SetRoleTier(roleName string, tier ModelTier) error {
	if roleName == "" {
		return errors.New("role name cannot be empty")
	}
	if !tier.Valid() {
		return ErrInvalidTier
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.setRoleTier(roleName, tier)
}

func (s *Store) setRoleTier(roleName string, tier ModelTier) error {
	rm := RoleTierMapping{
		RoleName:    roleName,
		DefaultTier: tier,
	}
	rec, err := toRecord(rm)
	if err != nil {
		return err
	}
	_, err = s.rolesCol.GetByKey(roleName)
	if errors.Is(err, engine.ErrKeyNotFound) {
		_, _, err = s.rolesCol.InsertWithKey(roleName, rec)
		return err
	}
	if err != nil {
		return err
	}
	_, err = s.rolesCol.UpdateByKey(roleName, rec)
	return err
}

// --- Handover Settings ------------------------------------------------------

func handoverSettingsFromRecord(d map[string]any) (HandoverSettings, error) {
	b, err := json.Marshal(d)
	if err != nil {
		return HandoverSettings{}, err
	}
	var out HandoverSettings
	if err := json.Unmarshal(b, &out); err != nil {
		return HandoverSettings{}, err
	}
	return out, nil
}

// GetHandoverSettings returns the current handover configuration, or the default settings if unconfigured.
func (s *Store) GetHandoverSettings() (HandoverSettings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getHandoverSettings()
}

func (s *Store) getHandoverSettings() (HandoverSettings, error) {
	defaults := DefaultHandoverSettings()
	r, err := s.handoverCol.GetByKey(HandoverSettingsKey)
	if errors.Is(err, engine.ErrKeyNotFound) {
		return defaults, nil
	}
	if err != nil {
		return HandoverSettings{}, err
	}
	hs, err := handoverSettingsFromRecord(r.Data)
	if err != nil {
		return HandoverSettings{}, err
	}
	return hs, nil
}

// SetHandoverSettings persists the handover configuration in the store.
func (s *Store) SetHandoverSettings(settings HandoverSettings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.setHandoverSettings(settings)
}

func (s *Store) setHandoverSettings(settings HandoverSettings) error {
	rec, err := toRecord(settings)
	if err != nil {
		return err
	}
	_, err = s.handoverCol.GetByKey(HandoverSettingsKey)
	if errors.Is(err, engine.ErrKeyNotFound) {
		_, _, err = s.handoverCol.InsertWithKey(HandoverSettingsKey, rec)
		return err
	}
	if err != nil {
		return err
	}
	_, err = s.handoverCol.UpdateByKey(HandoverSettingsKey, rec)
	return err
}

// --- Seeding ----------------------------------------------------------------

func (s *Store) seedDefaultsIfEmpty() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Seed any missing default models
	for _, m := range DefaultModels() {
		key := modelKey(m.BackendID, m.ModelID)
		rec, err := s.modelsCol.GetByKey(key)
		if errors.Is(err, engine.ErrKeyNotFound) {
			if err := s.upsertModel(m); err != nil {
				return err
			}
		} else if err == nil {
			if _, ok := rec.Data["auto_assign"]; !ok {
				// Backfill missing AutoAssign for existing records
				model, err := modelFromRecord(rec.Data)
				if err == nil {
					model.AutoAssign = m.AutoAssign
					s.upsertModel(model)
				}
			}
		} else if err != nil {
			return err
		}
	}

	// Seed any missing default role tiers
	for _, rm := range DefaultRoleTiers() {
		_, err := s.rolesCol.GetByKey(rm.RoleName)
		if errors.Is(err, engine.ErrKeyNotFound) {
			if err := s.setRoleTier(rm.RoleName, rm.DefaultTier); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
	}

	// Seed handover settings if unconfigured
	_, err := s.handoverCol.GetByKey(HandoverSettingsKey)
	if errors.Is(err, engine.ErrKeyNotFound) {
		if err := s.setHandoverSettings(DefaultHandoverSettings()); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}

	// Seed any missing default quotas
	for _, q := range DefaultQuotas() {
		_, err := s.quotasCol.GetByKey(q.BackendID)
		if errors.Is(err, engine.ErrKeyNotFound) {
			if err := s.upsertQuota(q); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
	}

	return nil
}

// Close flushes the ScrivaDB index and stops its background compaction goroutine.
func (s *Store) Close() error {
	return s.db.Close()
}
