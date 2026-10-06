package daemon

import (
	"fmt"
	"sort"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/projectstore"
)

// Identity conflict kinds reported by ReconcileProjectMembership.
const (
	// ConflictDuplicateAgentID: one logical agent id is held by more than one
	// record (twice in the active set, or in both the active and archived sets).
	ConflictDuplicateAgentID = "duplicate_agent_id"
	// ConflictAmbiguousMembership: more than one project's authoritative agents[]
	// claims the same agent id, so its history has no single owner.
	ConflictAmbiguousMembership = "ambiguous_membership"
)

// IdentityConflict is an explicit, operator-visible ambiguity. Reconcile never
// resolves one: conflicted ids are excluded from every write (no restamp, no
// backfill, no overwrite of one agent by another) until a human decides.
type IdentityConflict struct {
	Kind   string
	ID     string
	Detail string
}

// detectIdentityConflicts finds ids that cannot be trusted as a verified
// identity. Results are sorted for deterministic reports. Dangling project
// members (ids present in no store) are intentionally NOT conflicts.
func detectIdentityConflicts(active, archived []*agentstore.Agent, projs []projectstore.Project) []IdentityConflict {
	var out []IdentityConflict
	activeN, archivedN := map[string]int{}, map[string]int{}
	for _, a := range active {
		if a != nil && a.ID != "" {
			activeN[a.ID]++
		}
	}
	for _, a := range archived {
		if a != nil && a.ID != "" {
			archivedN[a.ID]++
		}
	}
	seen := map[string]bool{}
	for id, n := range activeN {
		if n > 1 || archivedN[id] > 0 {
			seen[id] = true
			out = append(out, IdentityConflict{ConflictDuplicateAgentID, id,
				fmt.Sprintf("%d active and %d archived records share this id", n, archivedN[id])})
		}
	}
	for id, n := range archivedN {
		if n > 1 && !seen[id] {
			out = append(out, IdentityConflict{ConflictDuplicateAgentID, id,
				fmt.Sprintf("%d archived records share this id", n)})
		}
	}
	claims := map[string][]string{}
	for _, p := range projs {
		for _, id := range sortedDedupe(append([]string(nil), p.Agents...)) {
			claims[id] = append(claims[id], p.ID)
		}
	}
	for id, owners := range claims {
		if len(owners) > 1 {
			sort.Strings(owners)
			out = append(out, IdentityConflict{ConflictAmbiguousMembership, id,
				fmt.Sprintf("claimed by projects %v", owners)})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ID != out[j].ID {
			return out[i].ID < out[j].ID
		}
		return out[i].Kind < out[j].Kind
	})
	return out
}

func conflictedIDs(cs []IdentityConflict) map[string]bool {
	m := make(map[string]bool, len(cs))
	for _, c := range cs {
		m[c.ID] = true
	}
	return m
}
