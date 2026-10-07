package e2e

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"

	"github.com/stretchr/testify/require"
)

type idxEntry struct {
	SegmentPath string `json:"segment"`
	Offset      int64  `json:"offset"`
	Rev         uint64 `json:"rev,omitempty"`
	ExpiresAt   int64  `json:"expires_at,omitempty"`
}

// rewriteIndex edits a collection's persisted primary index and re-signs it
// with a VALID checksum, so the engine accepts it on open and only identity
// verification can notice. The daemon MUST be stopped.
func (e *env) rewriteIndex(col string, edit func(map[string]idxEntry)) {
	e.t.Helper()
	path := filepath.Join(e.data, "agents-db", col, "index.json")
	b, err := os.ReadFile(path)
	require.NoError(e.t, err)
	var file struct {
		Entries  map[string]idxEntry `json:"entries"`
		Checksum string              `json:"checksum"`
	}
	require.NoError(e.t, json.Unmarshal(b, &file))
	edit(file.Entries)
	payload, err := json.Marshal(file.Entries)
	require.NoError(e.t, err)
	sum := sha256.Sum256(payload)
	file.Checksum = hex.EncodeToString(sum[:])
	out, err := json.Marshal(file)
	require.NoError(e.t, err)
	require.NoError(e.t, os.WriteFile(path, out, 0o644))
}

// byOffset returns the index keys ordered by record offset (insertion order).
func byOffset(m map[string]idxEntry) []string {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return m[ids[i]].Offset < m[ids[j]].Offset })
	return ids
}

// faults are the on-disk corruptions that open cleanly but read wrong.
var faults = map[string]func(*env){
	// one entry points at another record's (in-range, valid) bytes: wrong identity.
	"in-range-wrong-record": func(e *env) {
		e.rewriteIndex("agents", func(m map[string]idxEntry) { ids := byOffset(m); m[ids[0]] = m[ids[1]] })
	},
	// offset lands inside a record (in range, not a record boundary).
	"in-range-mid-record-offset": func(e *env) {
		e.rewriteIndex("agents", func(m map[string]idxEntry) {
			ids := byOffset(m)
			x := m[ids[0]]
			x.Offset += 3
			m[ids[0]] = x
		})
	},
	// last record's offset skewed by one byte (silent scan omission shape).
	"partial-scan-skewed-offset": func(e *env) {
		e.rewriteIndex("agents", func(m map[string]idxEntry) {
			ids := byOffset(m)
			x := m[ids[2]]
			x.Offset++
			m[ids[2]] = x
		})
	},
}
