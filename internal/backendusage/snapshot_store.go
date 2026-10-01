package backendusage

import (
	"bufio"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// SnapshotRetention bounds the append-only observation history. Latest known
// data is retained even when a failed observation follows it, and survives a
// daemon restart. Old observations are pruned on each write.
const SnapshotRetention = 30 * 24 * time.Hour

type SnapshotStore struct {
	mu        sync.Mutex
	path      string
	retention time.Duration
}

func NewSnapshotStore(dir string) (*SnapshotStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &SnapshotStore{path: filepath.Join(dir, "usage-snapshots.jsonl"), retention: SnapshotRetention}, nil
}

// Record persists every fetch. An unsuccessful/partial observation inherits the
// prior domain buckets as unknown rather than erasing or zeroing known capacity.
func (s *SnapshotStore) Record(in UsageSnapshot) (UsageSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.readLocked()
	if err != nil {
		return UsageSnapshot{}, err
	}
	var previous *UsageSnapshot
	for i := range all {
		if all[i].Domain.Key() == in.Domain.Key() && (previous == nil || all[i].Revision > previous.Revision) {
			previous = &all[i]
		}
	}
	var max uint64
	for _, v := range all {
		if v.Revision > max {
			max = v.Revision
		}
	}
	in.Revision = max + 1
	if in.RecordedAt.IsZero() {
		in.RecordedAt = time.Now().UTC()
	}
	if previous != nil {
		prior := make(map[string]CapacityBucket, len(previous.Buckets))
		for _, b := range previous.Buckets {
			prior[b.Key] = b
		}
		if !in.Authoritative && len(in.Buckets) == 0 {
			in.Buckets = cloneBuckets(previous.Buckets)
			for i := range in.Buckets {
				in.Buckets[i].State = BucketUnknown
			}
		} else {
			for i := range in.Buckets {
				if in.Buckets[i].State != BucketUnknown {
					continue
				}
				if old, ok := prior[in.Buckets[i].Key]; ok {
					// A partial result is diagnostic-only: retain the last measured
					// values but never present them as fresh authority.
					in.Buckets[i] = old
					in.Buckets[i].State = BucketUnknown
				}
			}
		}
	}
	all = append(all, in)
	cutoff := in.RecordedAt.Add(-s.retention)
	kept := all[:0]
	for _, v := range all {
		if !v.RecordedAt.Before(cutoff) {
			kept = append(kept, v)
		}
	}
	if err := s.writeLocked(kept); err != nil {
		return UsageSnapshot{}, err
	}
	return in, nil
}

func (s *SnapshotStore) Latest(domain CapacityDomain, now time.Time, staleAfter time.Duration) (UsageSnapshot, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.readLocked()
	if err != nil {
		return UsageSnapshot{}, false, err
	}
	var out UsageSnapshot
	found := false
	for _, v := range all {
		if v.Domain.Key() == domain.Key() && (!found || v.Revision > out.Revision) {
			out, found = v, true
		}
	}
	if !found {
		return UsageSnapshot{}, false, nil
	}
	if !out.Authoritative || out.ObservedAt.IsZero() {
		out.Freshness = FreshnessUnknown
	} else if now.UTC().Sub(out.ObservedAt) > staleAfter {
		out.Freshness = FreshnessStale
	} else {
		out.Freshness = FreshnessFresh
	}
	return out, true, nil
}

func (s *SnapshotStore) readLocked() ([]UsageSnapshot, error) {
	f, err := os.Open(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []UsageSnapshot
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		var v UsageSnapshot
		if json.Unmarshal(sc.Bytes(), &v) == nil && v.Domain.Provider != "" {
			out = append(out, v)
		}
	}
	return out, sc.Err()
}
func (s *SnapshotStore) writeLocked(all []UsageSnapshot) error {
	sort.Slice(all, func(i, j int) bool { return all[i].Revision < all[j].Revision })
	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	for _, v := range all {
		b, e := json.Marshal(v)
		if e != nil {
			f.Close()
			return e
		}
		if _, e = f.Write(append(b, '\n')); e != nil {
			f.Close()
			return e
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
func cloneBuckets(in []CapacityBucket) []CapacityBucket {
	out := append([]CapacityBucket(nil), in...)
	for i := range out {
		out[i].UsedPercent = clonePtr(out[i].UsedPercent)
		out[i].RemainingPercent = clonePtr(out[i].RemainingPercent)
		out[i].DurationMinutes = clonePtr(out[i].DurationMinutes)
		out[i].ResetsAt = clonePtr(out[i].ResetsAt)
	}
	return out
}
