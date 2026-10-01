package plansync

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/srjn45/warden/internal/planstore"
)

// HubStore persists Hub PlanSync envelopes. It is deliberately separate from a
// daemon's canonical plan store: the Hub is a transport and discovery fabric,
// not another local Plan source of truth.
type HubStore interface {
	Push(context.Context, Envelope) (Envelope, error)
	Pull(context.Context, PullQuery) ([]Envelope, error)
	Close() error
}

// FileHubStore is a small, durable HubStore suitable for a single Hub process.
// Each mutation is atomically replaced on disk, so a restarted Hub preserves
// envelopes and their remote IDs / conflict tokens.
type FileHubStore struct {
	mu     sync.Mutex
	path   string
	byKey  map[string]Envelope
	now    func() time.Time
	randID func() (string, error)
}

// NewFileHubStore opens (or creates) the Hub envelope store under dir.
func NewFileHubStore(dir string) (*FileHubStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &FileHubStore{
		path:   filepath.Join(dir, "plan-sync-hub.json"),
		byKey:  make(map[string]Envelope),
		now:    time.Now,
		randID: newRemoteID,
	}
	if raw, err := os.ReadFile(s.path); err == nil {
		if len(raw) != 0 {
			if err := json.Unmarshal(raw, &s.byKey); err != nil {
				return nil, fmt.Errorf("plansync: decode hub store: %w", err)
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return s, nil
}

// Push creates an envelope or acknowledges an idempotent retry. A different
// conflict token for the same scope and plan is a structured OCC conflict.
func (s *FileHubStore) Push(ctx context.Context, env Envelope) (Envelope, error) {
	if err := ctx.Err(); err != nil {
		return Envelope{}, err
	}
	if err := validateEnvelope(env); err != nil {
		return Envelope{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := hubEnvelopeKey(env.Scope, env.PlanID)
	if prev, ok := s.byKey[key]; ok {
		if prev.ConflictToken != env.ConflictToken {
			return Envelope{}, &ConflictError{PlanID: env.PlanID, Expected: prev.ConflictToken, Actual: env.ConflictToken}
		}
		return prev, nil
	}
	id, err := s.randID()
	if err != nil {
		return Envelope{}, err
	}
	now := s.now().UTC()
	env.RemoteID = id
	env.SyncedAt = &now
	s.byKey[key] = env
	if err := s.persistLocked(); err != nil {
		delete(s.byKey, key)
		return Envelope{}, err
	}
	return env, nil
}

// Pull lists the envelopes that match the supplied scope and filters.
func (s *FileHubStore) Pull(ctx context.Context, q PullQuery) ([]Envelope, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Envelope, 0, len(s.byKey))
	for _, env := range s.byKey {
		if !matchScope(env, q.Scope) || (q.PlanID != "" && env.PlanID != q.PlanID) || !matchStatuses(env.Lifecycle, q.Statuses) {
			continue
		}
		out = append(out, env)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].PlanID == out[j].PlanID {
			return out[i].RemoteID < out[j].RemoteID
		}
		return out[i].PlanID < out[j].PlanID
	})
	return out, nil
}

// Close implements HubStore. FileHubStore has no open descriptor.
func (*FileHubStore) Close() error { return nil }

func (s *FileHubStore) persistLocked() error {
	raw, err := json.Marshal(s.byKey)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".plan-sync-hub-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, s.path)
}

func hubEnvelopeKey(scope Scope, planID string) string {
	return scope.OrganizationID + "\x00" + scope.TeamID + "\x00" + scope.ProjectID + "\x00" + planID
}

func newRemoteID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("plansync: generate remote id: %w", err)
	}
	return "ps_" + hex.EncodeToString(b), nil
}

// DiscoverHub is shared by the HTTP service and keeps the protocol's empty
// status default in one place.
func DiscoverHub(ctx context.Context, store HubStore, scope Scope, statuses []planstore.PlanStatus) ([]Envelope, error) {
	if len(statuses) == 0 {
		statuses = []planstore.PlanStatus{planstore.PlanStatusPending, planstore.PlanStatusInProgress}
	}
	return store.Pull(ctx, PullQuery{Scope: scope, Statuses: statuses})
}
