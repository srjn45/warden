package agentbackend

import "fmt"

// DefaultID is the backend used when no backend is selected (empty session
// Backend field). Claude Code is warden's reference implementation and default.
const DefaultID = "claude"

// registry holds every Backend that has registered itself (via an adapter's
// init). It is populated at import time, so it is effectively read-only after
// program start and needs no locking.
var registry = map[string]Backend{}

// Register adds b to the registry under its ID. Adapters call this from init();
// a later registration under the same ID wins (last writer), which lets a test
// or an embedder override a built-in backend.
func Register(b Backend) { registry[b.ID()] = b }

// Get returns the backend registered under id. An empty id resolves to the
// default ("claude"), so a session with no explicit backend reads as Claude
// (back-compat). An unknown id is an error.
func Get(id string) (Backend, error) {
	if id == "" {
		id = DefaultID
	}
	b, ok := registry[id]
	if !ok {
		return nil, fmt.Errorf("unknown agent backend %q", id)
	}
	return b, nil
}

// Default returns the default backend, or nil if no adapter registered it (a
// wiring bug — the composition root must import the backends package so its
// init registers Claude).
func Default() Backend {
	return registry[DefaultID]
}

// IDs returns the registered backend ids in no particular order. Intended for
// diagnostics / help text.
func IDs() []string {
	ids := make([]string, 0, len(registry))
	for id := range registry {
		ids = append(ids, id)
	}
	return ids
}
