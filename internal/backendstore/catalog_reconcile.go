package backendstore

// The reconciliation service intentionally has no dependency on an adapter or
// process execution. Callers gather provider evidence first, then hand the
// immutable observation to Preview. This keeps provider I/O outside Store.mu
// and makes it impossible for a normal preview to alter routing rows.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/scriva/query"
)

var (
	ErrProposalNotFound = errors.New("catalog proposal not found")
	ErrProposalStale    = errors.New("catalog proposal is stale; re-preview required")
	ErrProposalExpired  = errors.New("catalog proposal has expired; re-preview required")
	ErrRevisionConflict = errors.New("catalog revision conflicts with later edits")
)

const catalogProposalTTL = 15 * time.Minute

// CatalogRefresh is a bounded discovery result prepared outside the store.
type CatalogRefresh struct {
	Scope       CatalogScope
	Observation CatalogObservation
	ObservedAt  time.Time
	SnapshotID  string // optional deterministic caller supplied evidence id
}

func hashModel(m *ModelEntry) string {
	if m == nil {
		return ""
	}
	b, _ := json.Marshal(m)
	h := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(h[:])
}
func cloneModel(m ModelEntry) *ModelEntry {
	c := m
	c.OperatorSet = append([]string(nil), m.OperatorSet...)
	return &c
}
func proposalKey(ai, id string) string { return ai + ":" + id }

// Preview records only evidence and a proposal. It does not change ModelEntry,
// quota, router state, or sessions. In particular, an incomplete response can
// add positive observations but can never turn an absent model unavailable.
func (s *Store) Preview(refresh CatalogRefresh, now time.Time) (CatalogProposal, error) {
	started := time.Now()
	if refresh.Scope.AiCli == "" {
		return CatalogProposal{}, errors.New("catalog scope AI CLI cannot be empty")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if refresh.ObservedAt.IsZero() {
		refresh.ObservedAt = now
	}
	o := refresh.Observation
	if o.Completeness == "" {
		o.Completeness = CompletenessFailed
	}
	var changes []ObservationChange
	// Count every completed refresh attempt, including failures in the short
	// persistence phase. Provider I/O has already happened before Preview.
	defer func() { s.catalogMetrics.observeRefresh(refresh.Scope.AiCli, o, time.Since(started), changes) }()
	snap := CatalogSnapshot{ID: refresh.SnapshotID, AiCli: refresh.Scope.AiCli, ScopeFP: refresh.Scope.ScopeFP(), AuthMode: refresh.Scope.AuthMode, PlanLabel: refresh.Scope.PlanLabel, CLIVersion: refresh.Scope.CLIVersion, Source: o.Source, SourceDetail: o.SourceDetail, SourceFetchedAt: o.SourceFetchedAt, ObservedAt: refresh.ObservedAt, Completeness: o.Completeness, Err: o.Err, Models: append([]ObservedModel(nil), o.Models...)}
	if snap.ID == "" {
		snap.ID = snapshotID(snap)
	}
	b, _ := json.Marshal(snap.Models)
	sum := sha256.Sum256(b)
	snap.ContentHash = "sha256:" + hex.EncodeToString(sum[:])

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.putCatalogSnapshotLocked(snap); err != nil {
		return CatalogProposal{}, err
	}
	changes, err := s.applyEvidenceLocked(refresh.Scope, snap, now)
	if err != nil {
		return CatalogProposal{}, err
	}
	models, err := s.listModels("")
	if err != nil {
		return CatalogProposal{}, err
	}
	head, err := s.catalogHeadLocked()
	if err != nil {
		return CatalogProposal{}, err
	}
	p := CatalogProposal{State: "open", CreatedAt: now, ExpiresAt: now.Add(catalogProposalTTL), AiClis: []string{refresh.Scope.AiCli}, Snapshots: []string{snap.ID}, BaseRev: head, PolicyVersion: ModelPolicyVersion, Scope: ActiveCatalogScope{AiCli: refresh.Scope.AiCli, ActiveScopeFP: refresh.Scope.ScopeFP(), AuthMode: refresh.Scope.AuthMode, PlanLabel: refresh.Scope.PlanLabel, CLIVersion: refresh.Scope.CLIVersion, ResolvedAt: now}, ScopeFP: refresh.Scope.ScopeFP(), Observations: changes}
	p.Ops, p.Blocked = s.recommendLocked(refresh.Scope, snap, models, now)
	p.ID = proposalID(p)
	// A newer preview replaces overlapping open proposals; it never silently
	// applies them later.
	if err := s.supersedeOpenLocked(p.AiClis, p.ID); err != nil {
		return CatalogProposal{}, err
	}
	if err := putCatalog(s.catalogProposalsCol, p.ID, p); err != nil {
		return CatalogProposal{}, err
	}
	return p, nil
}

func snapshotID(s CatalogSnapshot) string {
	h := sha256.Sum256([]byte(s.AiCli + "|" + s.ScopeFP + "|" + s.ObservedAt.UTC().Format(time.RFC3339Nano) + "|" + string(s.Completeness)))
	return "snap-" + hex.EncodeToString(h[:10])
}
func proposalID(p CatalogProposal) string {
	q := p
	q.ID = ""
	q.CreatedAt = time.Time{}
	q.ExpiresAt = time.Time{}
	b, _ := json.Marshal(q)
	h := sha256.Sum256(b)
	return "prop-" + hex.EncodeToString(h[:10])
}

func (s *Store) putCatalogSnapshotLocked(snapshot CatalogSnapshot) error {
	if prior, err := s.catalogSnapshotsCol.GetByKey(snapshot.ID); err == nil {
		existing, e := catalogRecord[CatalogSnapshot](prior.Data)
		if e != nil {
			return e
		}
		if existing.ContentHash != snapshot.ContentHash {
			return errors.New("catalog snapshots are immutable")
		}
		return nil
	}
	if err := putCatalog(s.catalogSnapshotsCol, snapshot.ID, snapshot); err != nil {
		return err
	}
	return s.pruneCatalogSnapshotsLocked(time.Now())
}

func (s *Store) applyEvidenceLocked(scope CatalogScope, snap CatalogSnapshot, now time.Time) ([]ObservationChange, error) {
	prior, err := s.listCatalogObservationsLocked(scope.AiCli, scope.ScopeFP())
	if err != nil {
		return nil, err
	}
	present := map[string]ObservedModel{}
	for _, m := range snap.Models {
		if strings.TrimSpace(m.ModelID) != "" {
			present[m.ModelID] = m
		}
	}
	ids := map[string]bool{}
	for id := range prior {
		ids[id] = true
	}
	for id := range present {
		ids[id] = true
	}
	var out []ObservationChange
	for id := range ids {
		old := prior[id]
		from := old.Availability
		next := evidenceAvailability(old, snap, id, present[id], now)
		if next == "" {
			continue
		}
		if from == "" {
			from = AvailabilityUnknown
		}
		n := old
		n.AiCli = scope.AiCli
		n.ScopeFP = scope.ScopeFP()
		n.ModelID = id
		n.Availability = next
		n.LastCheckedAt = now
		n.EvidenceSnapshot = snap.ID
		if m, ok := present[id]; ok {
			n.LastSeenAt = now
			n.Visibility = m.Visibility
			n.Capabilities = ModelCapabilities{ReasoningLevels: append([]string(nil), m.ReasoningLevels...), DefaultReasoning: m.DefaultReasoning, ContextWindow: m.ContextWindow, UpgradeTo: m.UpgradeTo}
		}
		if err := putCatalog(s.catalogObservationsCol, ObservationKey(n.AiCli, n.ScopeFP, n.ModelID), n); err != nil {
			return nil, err
		}
		if from != next {
			out = append(out, ObservationChange{Key: ObservationKey(n.AiCli, n.ScopeFP, n.ModelID), From: from, To: next})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func evidenceAvailability(old StoredObservation, snap CatalogSnapshot, id string, m ObservedModel, now time.Time) Availability {
	_, present := map[string]ObservedModel{id: m}[id] // presence is decided by caller through model id
	// The zero value m is safe because actual model ids are non-empty. The caller
	// passes only ids in its union; inspect membership by matching id.
	present = m.ModelID != ""
	if present {
		return AvailabilityAvailable
	}
	if snap.Completeness == CompletenessComplete && snap.Err == nil && snapshotFresh(snap, now) {
		return AvailabilityUnavailable
	}
	if old.Availability == AvailabilityAvailable && !old.LastSeenAt.IsZero() && now.Sub(old.LastSeenAt) > 24*time.Hour {
		return AvailabilityStale
	}
	if old.Availability != "" {
		return old.Availability
	}
	return AvailabilityUnknown
}
func snapshotFresh(s CatalogSnapshot, now time.Time) bool {
	if now.Sub(s.ObservedAt) > 24*time.Hour {
		return false
	}
	return s.Source != SourceCLICache || (!s.SourceFetchedAt.IsZero() && now.Sub(s.SourceFetchedAt) <= 24*time.Hour)
}

func (s *Store) listCatalogObservationsLocked(ai, fp string) (map[string]StoredObservation, error) {
	rows, err := s.catalogObservationsCol.Scan(query.MatchAll)
	if err != nil {
		return nil, err
	}
	out := map[string]StoredObservation{}
	for _, r := range rows {
		o, e := catalogRecord[StoredObservation](r.Data)
		if e == nil && o.AiCli == ai && o.ScopeFP == fp {
			out[o.ModelID] = o
		}
	}
	return out, nil
}

func (s *Store) recommendLocked(scope CatalogScope, snap CatalogSnapshot, models []ModelEntry, now time.Time) ([]CatalogProposalOp, []CatalogBlockedOp) {
	byID := map[string]ModelEntry{}
	for _, m := range models {
		if m.BackendID == scope.AiCli {
			byID[m.ModelID] = m
		}
	}
	seen := map[string]ObservedModel{}
	for _, m := range snap.Models {
		seen[m.ModelID] = m
	}
	ops := []CatalogProposalOp{}
	blocked := []CatalogBlockedOp{}
	for id, obs := range seen {
		prior, exists := byID[id]
		rec := RecommendModel(scope, obs, nil)
		if !exists {
			after := ModelEntry{BackendID: scope.AiCli, ModelID: id, DisplayName: obs.DisplayName, Tier: rec.Tier, AutoAssign: rec.AutoAssign, Enabled: true, IsCustom: true, Origin: ModelOriginDiscovered, Lifecycle: ModelLifecycleActive, QuotaScope: rec.QuotaScope}
			ops = append(ops, CatalogProposalOp{Op: "add", Key: modelKey(scope.AiCli, id), After: cloneModel(after), Reasons: append([]string(nil), rec.RuleIDs...), Evidence: snap.ID})
			continue
		}
		after := prior
		changed := false
		if obs.DisplayName != "" && obs.DisplayName != after.DisplayName {
			after.DisplayName = obs.DisplayName
			changed = true
		}
		if !hasOperatorSet(after, "tier") && after.Tier != rec.Tier {
			after.Tier = rec.Tier
			changed = true
		}
		if !hasOperatorSet(after, "auto_assign") && after.AutoAssign != rec.AutoAssign {
			after.AutoAssign = rec.AutoAssign
			changed = true
		}
		if !hasOperatorSet(after, "quota_scope") && rec.QuotaScope != "" && after.QuotaScope != rec.QuotaScope {
			after.QuotaScope = rec.QuotaScope
			changed = true
		}
		if after.EffectiveLifecycle() == ModelLifecycleRetired {
			after.Lifecycle = ModelLifecycleActive
			after.AutoAssign = after.RetiredPrior.AutoAssign
			after.RetiredPrior = nil
			changed = true
		}
		if changed {
			kind := "recommend"
			if after.DisplayName != prior.DisplayName && len(rec.RuleIDs) == 0 {
				kind = "update_metadata"
			}
			ops = append(ops, CatalogProposalOp{Op: kind, Key: modelKey(scope.AiCli, id), PreHash: hashModel(&prior), Before: cloneModel(prior), After: cloneModel(after), Reasons: append([]string(nil), rec.RuleIDs...), Evidence: snap.ID})
		}
	}
	// Only complete, authoritative fresh account evidence may retire absences.
	if snap.Completeness != CompletenessComplete || snap.Err != nil || !snapshotFresh(snap, now) {
		for _, m := range byID {
			if m.EffectiveLifecycle() == ModelLifecycleActive {
				blocked = append(blocked, CatalogBlockedOp{Key: modelKey(m.BackendID, m.ModelID), Op: "retire", Reason: "incomplete_or_stale_snapshot"})
			}
		}
	} else {
		candidates := []ModelEntry{}
		for _, m := range byID {
			if m.EffectiveLifecycle() == ModelLifecycleActive {
				if _, ok := seen[m.ModelID]; !ok {
					candidates = append(candidates, m)
				}
			}
		}
		if len(candidates)*2 > len(byID) {
			for _, m := range candidates {
				blocked = append(blocked, CatalogBlockedOp{Key: modelKey(m.BackendID, m.ModelID), Op: "retire", Reason: "mass_retirement_guard"})
			}
		} else {
			for _, m := range candidates {
				if len(m.OperatorSet) > 0 {
					blocked = append(blocked, CatalogBlockedOp{Key: modelKey(m.BackendID, m.ModelID), Op: "retire", Reason: "operator_set"})
					continue
				}
				after := m
				after.Lifecycle = ModelLifecycleRetired
				after.RetiredAt = now
				after.RetiredPrior = &RetiredPrior{AutoAssign: m.AutoAssign}
				after.AutoAssign = false
				ops = append(ops, CatalogProposalOp{Op: "retire", Key: modelKey(m.BackendID, m.ModelID), PreHash: hashModel(&m), Before: cloneModel(m), After: cloneModel(after), Reasons: []string{"complete_account_scoped_absence"}, Evidence: snap.ID})
			}
		}
	}
	sort.Slice(ops, func(i, j int) bool { return ops[i].Key < ops[j].Key })
	sort.Slice(blocked, func(i, j int) bool { return blocked[i].Key < blocked[j].Key })
	return ops, blocked
}

// Apply is deliberately authorization-shaped: callers must name the exact
// proposal they previewed. A changed head, account scope, row, expiry, or prior
// proposal state fails closed and requires a new preview.
func (s *Store) ApplyCatalogProposal(id string, now time.Time) (out CatalogRevision, err error) {
	defer func() { s.catalogMetrics.observeConflict(err) }()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.catalogProposalLocked(id)
	if err != nil {
		return CatalogRevision{}, err
	}
	if p.State != "open" {
		return CatalogRevision{}, ErrProposalStale
	}
	if !p.ExpiresAt.IsZero() && now.After(p.ExpiresAt) {
		p.State = "expired"
		_ = putCatalog(s.catalogProposalsCol, p.ID, p)
		return CatalogRevision{}, ErrProposalExpired
	}
	head, err := s.catalogHeadLocked()
	if err != nil {
		return CatalogRevision{}, err
	}
	if head != p.BaseRev {
		return CatalogRevision{}, ErrProposalStale
	}
	for _, op := range p.Ops {
		cur, err := s.getModelByKeyLocked(op.Key)
		if op.PreHash == "" {
			if err == nil {
				return CatalogRevision{}, ErrProposalStale
			}
			if !errors.Is(err, ErrModelNotFound) {
				return CatalogRevision{}, err
			}
			continue
		}
		if err != nil || hashModel(&cur) != op.PreHash {
			return CatalogRevision{}, ErrProposalStale
		}
	}
	// All guards were checked before the first mutation. Store.mu serializes all
	// registry RMW paths, so no manual edit can interleave this commit.
	next := head + 1
	for _, op := range p.Ops {
		if op.After == nil {
			continue
		}
		after := *op.After
		after.CatalogRev = next
		if err := s.upsertModel(after); err != nil {
			return CatalogRevision{}, err
		}
	}
	if err := putCatalog(s.catalogScopesCol, p.Scope.AiCli, p.Scope); err != nil {
		return CatalogRevision{}, err
	}
	r := CatalogRevision{Rev: next, ParentRev: head, CreatedAt: now, Snapshots: append([]string(nil), p.Snapshots...), Kind: "apply", ProposalID: p.ID, Ops: cloneOps(p.Ops)}
	if err := putCatalog(s.catalogRevisionsCol, revisionKey(next), r); err != nil {
		return CatalogRevision{}, err
	}
	if err := putCatalog(s.catalogRevisionsCol, "__head__", CatalogRevision{Rev: next}); err != nil {
		return CatalogRevision{}, err
	}
	p.State = "applied"
	if err := putCatalog(s.catalogProposalsCol, p.ID, p); err != nil {
		return CatalogRevision{}, err
	}
	return r, nil
}

// RollbackCatalogRevision restores catalog policy rows only. It intentionally
// never touches quota usage, observations, snapshots, routes already chosen, or
// running sessions.
func (s *Store) RollbackCatalogRevision(rev uint64, now time.Time) (out CatalogRevision, err error) {
	defer func() { s.catalogMetrics.observeConflict(err) }()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	target, err := s.catalogRevisionLocked(rev)
	if err != nil {
		return CatalogRevision{}, err
	}
	head, err := s.catalogHeadLocked()
	if err != nil {
		return CatalogRevision{}, err
	}
	for _, op := range target.Ops {
		cur, e := s.getModelByKeyLocked(op.Key)
		if op.After == nil {
			if e == nil {
				return CatalogRevision{}, ErrRevisionConflict
			}
			continue
		}
		if e != nil || hashModel(&cur) != hashModel(op.After) {
			return CatalogRevision{}, fmt.Errorf("%w: %s", ErrRevisionConflict, op.Key)
		}
	}
	next := head + 1
	inverse := make([]CatalogProposalOp, 0, len(target.Ops))
	for _, op := range target.Ops {
		cur, _ := s.getModelByKeyLocked(op.Key)
		inv := CatalogProposalOp{Op: "rollback", Key: op.Key, PreHash: hashModel(&cur), Before: cloneModel(cur), After: op.Before, Reasons: []string{"rollback"}}
		if op.Before == nil {
			if err := s.modelsCol.DeleteByKey(op.Key); err != nil && !errors.Is(err, engine.ErrKeyNotFound) {
				return CatalogRevision{}, err
			}
		} else {
			before := *op.Before
			before.CatalogRev = next
			if err := s.upsertModel(before); err != nil {
				return CatalogRevision{}, err
			}
		}
		inverse = append(inverse, inv)
	}
	r := CatalogRevision{Rev: next, ParentRev: head, CreatedAt: now, Kind: "rollback", Reverts: rev, Ops: inverse}
	if err := putCatalog(s.catalogRevisionsCol, revisionKey(next), r); err != nil {
		return CatalogRevision{}, err
	}
	if err := putCatalog(s.catalogRevisionsCol, "__head__", CatalogRevision{Rev: next}); err != nil {
		return CatalogRevision{}, err
	}
	return r, nil
}

func cloneOps(in []CatalogProposalOp) []CatalogProposalOp {
	out := make([]CatalogProposalOp, len(in))
	for i, v := range in {
		out[i] = v
		if v.Before != nil {
			out[i].Before = cloneModel(*v.Before)
		}
		if v.After != nil {
			out[i].After = cloneModel(*v.After)
		}
		out[i].Reasons = append([]string(nil), v.Reasons...)
	}
	return out
}
func (s *Store) catalogProposalLocked(id string) (CatalogProposal, error) {
	r, e := s.catalogProposalsCol.GetByKey(id)
	if errors.Is(e, engine.ErrKeyNotFound) {
		return CatalogProposal{}, ErrProposalNotFound
	}
	if e != nil {
		return CatalogProposal{}, e
	}
	return catalogRecord[CatalogProposal](r.Data)
}
func (s *Store) catalogRevisionLocked(rev uint64) (CatalogRevision, error) {
	r, e := s.catalogRevisionsCol.GetByKey(revisionKey(rev))
	if errors.Is(e, engine.ErrKeyNotFound) {
		return CatalogRevision{}, ErrNotFound
	}
	if e != nil {
		return CatalogRevision{}, e
	}
	return catalogRecord[CatalogRevision](r.Data)
}
func (s *Store) catalogHeadLocked() (uint64, error) {
	r, e := s.catalogRevisionsCol.GetByKey("__head__")
	if errors.Is(e, engine.ErrKeyNotFound) {
		return 0, nil
	}
	if e != nil {
		return 0, e
	}
	v, e := catalogRecord[CatalogRevision](r.Data)
	return v.Rev, e
}
func (s *Store) getModelByKeyLocked(key string) (ModelEntry, error) {
	r, e := s.modelsCol.GetByKey(key)
	if errors.Is(e, engine.ErrKeyNotFound) {
		return ModelEntry{}, ErrModelNotFound
	}
	if e != nil {
		return ModelEntry{}, e
	}
	return modelFromRecord(r.Data)
}
func (s *Store) supersedeOpenLocked(aiClis []string, except string) error {
	rows, e := s.catalogProposalsCol.Scan(query.MatchAll)
	if e != nil {
		return e
	}
	wanted := map[string]bool{}
	for _, a := range aiClis {
		wanted[a] = true
	}
	for _, row := range rows {
		p, e := catalogRecord[CatalogProposal](row.Data)
		if e != nil || p.ID == except || p.State != "open" {
			continue
		}
		overlap := false
		for _, a := range p.AiClis {
			overlap = overlap || wanted[a]
		}
		if overlap {
			p.State = "superseded"
			if e := putCatalog(s.catalogProposalsCol, p.ID, p); e != nil {
				return e
			}
		}
	}
	return nil
}

func (s *Store) GetCatalogProposal(id string) (CatalogProposal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.catalogProposalLocked(id)
}
func (s *Store) GetCatalogRevision(rev uint64) (CatalogRevision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.catalogRevisionLocked(rev)
}
