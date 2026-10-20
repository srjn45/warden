package backendstore

// Persistence primitives for the model catalog. Discovery and policy live in
// later layers; this file intentionally only owns durable, account-scoped facts.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/scriva/query"
)

const catalogSnapshotRetention = 20

func hasOperatorSet(m ModelEntry, field string) bool {
	for _, v := range m.OperatorSet {
		if v == field {
			return true
		}
	}
	return false
}

// ObservationKey uses the model id verbatim unless that would exceed dbtx's
// 128-byte idempotency/key boundary. The full id remains in StoredObservation.
func ObservationKey(aiCli, scopeFP, modelID string) string {
	k := aiCli + "|" + scopeFP + "|" + modelID
	if len(k) <= 128 {
		return k
	}
	h := sha256.Sum256([]byte(modelID))
	return aiCli + "|" + scopeFP + "|" + hex.EncodeToString(h[:16])
}

func catalogRecord[T any](d map[string]any) (T, error) {
	var out T
	b, err := json.Marshal(d)
	if err != nil {
		return out, err
	}
	err = json.Unmarshal(b, &out)
	return out, err
}

func putCatalog[T any](col docs, key string, value T) error {
	rec, err := toRecord(value)
	if err != nil {
		return err
	}
	_, err = col.GetByKey(key)
	if errors.Is(err, engine.ErrKeyNotFound) {
		_, _, err = col.InsertWithKey(key, rec)
		return err
	}
	if err != nil {
		return err
	}
	_, err = col.UpdateByKey(key, rec)
	return err
}

func (s *Store) hasAvailableObservation(aiCli, modelID string) bool {
	if s.catalogObservationsCol == nil {
		return false
	}
	rows, err := s.catalogObservationsCol.Scan(query.MatchAll)
	if err != nil {
		return false
	}
	for _, r := range rows {
		o, err := catalogRecord[StoredObservation](r.Data)
		if err == nil && o.AiCli == aiCli && o.ModelID == modelID && o.Availability == AvailabilityAvailable {
			return true
		}
	}
	return false
}

func (s *Store) PutCatalogScope(scope ActiveCatalogScope) error {
	if strings.TrimSpace(scope.AiCli) == "" {
		return errors.New("catalog scope AI CLI cannot be empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return putCatalog(s.catalogScopesCol, scope.AiCli, scope)
}

func (s *Store) GetCatalogScope(aiCli string) (ActiveCatalogScope, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.catalogScopesCol.GetByKey(aiCli)
	if errors.Is(err, engine.ErrKeyNotFound) {
		return ActiveCatalogScope{}, ErrNotFound
	}
	if err != nil {
		return ActiveCatalogScope{}, err
	}
	return catalogRecord[ActiveCatalogScope](r.Data)
}

func (s *Store) PutCatalogObservation(observation StoredObservation) error {
	if observation.AiCli == "" || observation.ModelID == "" {
		return errors.New("catalog observation AI CLI and model ID cannot be empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return putCatalog(s.catalogObservationsCol, ObservationKey(observation.AiCli, observation.ScopeFP, observation.ModelID), observation)
}

func (s *Store) GetCatalogObservation(aiCli, scopeFP, modelID string) (StoredObservation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.catalogObservationsCol.GetByKey(ObservationKey(aiCli, scopeFP, modelID))
	if errors.Is(err, engine.ErrKeyNotFound) {
		return StoredObservation{}, ErrNotFound
	}
	if err != nil {
		return StoredObservation{}, err
	}
	return catalogRecord[StoredObservation](r.Data)
}

// GetActiveCatalogObservation returns the evidence for the currently active
// account scope. Missing scope/evidence is intentionally reported as unknown:
// it must not be mistaken for fresh quota or account entitlement.
func (s *Store) GetActiveCatalogObservation(aiCli, modelID string) (StoredObservation, error) {
	scope, err := s.GetCatalogScope(aiCli)
	if err == ErrNotFound || scope.ActiveScopeFP == "" {
		return StoredObservation{AiCli: aiCli, ModelID: modelID, Availability: AvailabilityUnknown}, nil
	}
	if err != nil {
		return StoredObservation{AiCli: aiCli, ModelID: modelID, Availability: AvailabilityUnknown}, err
	}
	o, err := s.GetCatalogObservation(aiCli, scope.ActiveScopeFP, modelID)
	if err == ErrNotFound {
		return StoredObservation{AiCli: aiCli, ModelID: modelID, Availability: AvailabilityUnknown}, nil
	}
	return o, err
}

// PutCatalogSnapshot persists an immutable evidence snapshot. Replaying the
// same ID is idempotent; retention is invoked only on refresh writes, never on
// startup.
func (s *Store) PutCatalogSnapshot(snapshot CatalogSnapshot) error {
	if snapshot.ID == "" || snapshot.AiCli == "" {
		return errors.New("catalog snapshot ID and AI CLI cannot be empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if snapshot.SchemaVersion == 0 {
		snapshot.SchemaVersion = 1
	}
	if prior, err := s.catalogSnapshotsCol.GetByKey(snapshot.ID); err == nil {
		existing, derr := catalogRecord[CatalogSnapshot](prior.Data)
		if derr != nil {
			return derr
		}
		oldJSON, _ := json.Marshal(existing)
		newJSON, _ := json.Marshal(snapshot)
		if string(oldJSON) != string(newJSON) {
			return errors.New("catalog snapshots are immutable")
		}
		return nil
	} else if !errors.Is(err, engine.ErrKeyNotFound) {
		return err
	}
	if err := putCatalog(s.catalogSnapshotsCol, snapshot.ID, snapshot); err != nil {
		return err
	}
	return s.pruneCatalogSnapshotsLocked(time.Now())
}

func (s *Store) ListCatalogSnapshots(aiCli, scopeFP string) ([]CatalogSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listCatalogSnapshotsLocked(aiCli, scopeFP)
}

// ListCatalogRevisions returns catalog policy history oldest-first.  History is
// operational metadata: unlike snapshots it does not include account secrets.
func (s *Store) ListCatalogRevisions() ([]CatalogRevision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.catalogRevisionsCol.Scan(query.MatchAll)
	if err != nil {
		return nil, err
	}
	out := make([]CatalogRevision, 0, len(rows))
	for _, row := range rows {
		v, err := catalogRecord[CatalogRevision](row.Data)
		// The head marker has no timestamp or kind; scan records do not expose
		// their collection key, so identify the marker from its reserved shape.
		if err == nil && (!v.CreatedAt.IsZero() || v.Kind != "") {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Rev < out[j].Rev })
	return out, nil
}

func (s *Store) listCatalogSnapshotsLocked(aiCli, scopeFP string) ([]CatalogSnapshot, error) {
	rows, err := s.catalogSnapshotsCol.Scan(query.MatchAll)
	if err != nil {
		return nil, err
	}
	out := make([]CatalogSnapshot, 0, len(rows))
	for _, row := range rows {
		v, err := catalogRecord[CatalogSnapshot](row.Data)
		if err == nil && (aiCli == "" || v.AiCli == aiCli) && (scopeFP == "" || v.ScopeFP == scopeFP) {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ObservedAt.After(out[j].ObservedAt) })
	return out, nil
}

func (s *Store) PutCatalogProposal(proposal CatalogProposal) error {
	if proposal.ID == "" {
		return errors.New("catalog proposal ID cannot be empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return putCatalog(s.catalogProposalsCol, proposal.ID, proposal)
}

func (s *Store) PutCatalogRevision(revision CatalogRevision) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return putCatalog(s.catalogRevisionsCol, revisionKey(revision.Rev), revision)
}

func revisionKey(rev uint64) string { return "rev-" + fmtUint20(rev) }
func fmtUint20(v uint64) string {
	b := make([]byte, 20)
	for i := len(b) - 1; i >= 0; i-- {
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b)
}

func (s *Store) pruneCatalogSnapshotsLocked(now time.Time) error {
	rows, err := s.catalogSnapshotsCol.Scan(query.MatchAll)
	if err != nil {
		return err
	}
	groups := map[string][]CatalogSnapshot{}
	for _, row := range rows {
		snap, err := catalogRecord[CatalogSnapshot](row.Data)
		if err == nil {
			groups[snap.AiCli+"\x00"+snap.ScopeFP] = append(groups[snap.AiCli+"\x00"+snap.ScopeFP], snap)
		}
	}
	protected := s.referencedSnapshotsLocked()
	cutoff := now.Add(-30 * 24 * time.Hour)
	for _, snaps := range groups {
		sort.Slice(snaps, func(i, j int) bool { return snaps[i].ObservedAt.After(snaps[j].ObservedAt) })
		for i, snap := range snaps {
			if i >= catalogSnapshotRetention && snap.ObservedAt.Before(cutoff) && !protected[snap.ID] {
				if err := s.catalogSnapshotsCol.DeleteByKey(snap.ID); err != nil && !errors.Is(err, engine.ErrKeyNotFound) {
					return err
				}
			}
		}
	}
	return nil
}

func (s *Store) referencedSnapshotsLocked() map[string]bool {
	keep := map[string]bool{}
	for _, col := range []docs{s.catalogProposalsCol, s.catalogRevisionsCol} {
		rows, err := col.Scan(query.MatchAll)
		if err != nil {
			continue
		}
		for _, row := range rows {
			var refs struct {
				Snapshots []string `json:"snapshots"`
			}
			b, _ := json.Marshal(row.Data)
			if json.Unmarshal(b, &refs) == nil {
				for _, id := range refs.Snapshots {
					keep[id] = true
				}
			}
		}
	}
	return keep
}
