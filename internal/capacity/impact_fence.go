package capacity

import (
	"bufio"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// FenceRecord is one durable claim that a snapshot revision/domain/bucket has
// already selected an agent at a given recovery generation. Repeated identical
// observations and concurrent API/pane triggers share this fence.
type FenceRecord struct {
	SnapshotRevision   uint64 `json:"snapshot_revision"`
	DomainKey          string `json:"domain_key"`
	BucketKey          string `json:"bucket_key"`
	AgentID            string `json:"agent_id"`
	RecoveryGeneration uint64 `json:"recovery_generation"`
	Source             string `json:"source,omitempty"`
}

// DurableFenceStore is a restart-safe Claim implementation. Claims are written
// before an agent is returned as affected so a daemon crash mid-pass cannot
// double-select the same agent for the same incident after restart.
type DurableFenceStore struct {
	mu   sync.Mutex
	path string
}

// NewDurableFenceStore opens (or creates) the fence log under dir.
func NewDurableFenceStore(dir string) (*DurableFenceStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &DurableFenceStore{path: filepath.Join(dir, "quota-impact-fences.jsonl")}, nil
}

// Claim records the fence when no prior claim matches the snapshot revision or
// the domain/bucket/agent/generation tuple. Returns claimed=false when a prior
// fence already covers the observation (idempotent).
func (s *DurableFenceStore) Claim(rec FenceRecord) (bool, error) {
	if s == nil {
		return true, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.readLocked()
	if err != nil {
		return false, err
	}
	for _, prev := range all {
		if sameRevisionFence(prev, rec) || sameGenerationFence(prev, rec) {
			return false, nil
		}
	}
	all = append(all, rec)
	if err := s.writeLocked(all); err != nil {
		return false, err
	}
	return true, nil
}

func sameRevisionFence(a, b FenceRecord) bool {
	return a.SnapshotRevision == b.SnapshotRevision &&
		a.DomainKey == b.DomainKey &&
		a.BucketKey == b.BucketKey &&
		a.AgentID == b.AgentID
}

func sameGenerationFence(a, b FenceRecord) bool {
	// Cross-source / multi-revision fence for one incident: same domain bucket
	// agent at the same recovery generation has already been selected.
	return a.DomainKey == b.DomainKey &&
		a.BucketKey == b.BucketKey &&
		a.AgentID == b.AgentID &&
		a.RecoveryGeneration == b.RecoveryGeneration
}

func (s *DurableFenceStore) readLocked() ([]FenceRecord, error) {
	f, err := os.Open(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []FenceRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		var v FenceRecord
		if json.Unmarshal(sc.Bytes(), &v) == nil && v.AgentID != "" && v.BucketKey != "" {
			out = append(out, v)
		}
	}
	return out, sc.Err()
}

func (s *DurableFenceStore) writeLocked(all []FenceRecord) error {
	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	for _, v := range all {
		if err := enc.Encode(v); err != nil {
			f.Close()
			return err
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
