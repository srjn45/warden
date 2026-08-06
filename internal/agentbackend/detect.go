package agentbackend

import (
	"os/exec"
	"sort"
)

// Detected reports one backend's presence on the daemon host: whether its binary
// resolves on PATH and, if so, where. It is the shared detection fact the backend
// registry reconciles into the store (docs/specs/2026-08-06-backend-registry.md
// §4) and the autopilot cost ladder both read.
type Detected struct {
	ID        string
	Binary    string
	Path      string
	Installed bool
}

// Detect probes every registered backend for its binary on PATH, returning one
// Detected per backend sorted by id (stable output for reconcile/diagnostics).
// It is a pure fact about the host — tiering/default/enabled preferences live in
// the store and are never read or written here.
func Detect() []Detected {
	ids := IDs()
	sort.Strings(ids)
	out := make([]Detected, 0, len(ids))
	for _, id := range ids {
		b, err := Get(id)
		if err != nil {
			continue
		}
		p, lookErr := exec.LookPath(b.Binary())
		out = append(out, Detected{ID: id, Binary: b.Binary(), Path: p, Installed: lookErr == nil})
	}
	return out
}
