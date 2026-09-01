package recovery

import (
	"sort"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/backendusage"
)

type Candidate struct {
	BackendID string
	ModelID   string
	Priority  int
	Headroom  *float64
	Resets    []Reset
}

type Reset struct {
	LimitID  string
	Scope    string
	ResetsAt *time.Time
}

// Rank overlays provider-owned windows onto an already policy-eligible set.
// Unknown usage remains eligible and sorts after known positive headroom.
func Rank(ids []Candidate, snap backendusage.Snapshot) []Candidate {
	byBackend := make(map[string]backendusage.BackendResult, len(snap.Backends))
	for _, b := range snap.Backends {
		byBackend[b.ID] = b
	}
	out := make([]Candidate, 0, len(ids))
	for _, c := range ids {
		row := byBackend[c.BackendID]
		var min *float64
		for _, limit := range row.Usage {
			if !applies(limit, c.ModelID) {
				continue
			}
			c.Resets = append(c.Resets, Reset{LimitID: limit.ID, Scope: limit.Scope, ResetsAt: cloneTime(limit.ResetsAt)})
			if limit.UsedPercent == nil {
				continue
			}
			h := 100 - *limit.UsedPercent
			if h < 0 {
				h = 0
			}
			if h > 100 {
				h = 100
			}
			if min == nil || h < *min {
				v := h
				min = &v
			}
		}
		c.Headroom = min
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		if (a.Headroom != nil) != (b.Headroom != nil) {
			return a.Headroom != nil
		}
		if a.Headroom != nil && *a.Headroom != *b.Headroom {
			return *a.Headroom > *b.Headroom
		}
		if a.BackendID != b.BackendID {
			return a.BackendID < b.BackendID
		}
		return a.ModelID < b.ModelID
	})
	return out
}

func applies(l backendusage.Limit, model string) bool {
	if len(l.Models) == 0 && len(l.ModelFamilies) == 0 {
		// A named negative family pool (for example Antigravity's non-gemini
		// scope) is still selective even when the provider has no exact ids.
		scope := strings.ToLower(l.Scope)
		if strings.HasPrefix(scope, "non-") {
			return !strings.Contains(strings.ToLower(model), strings.TrimPrefix(scope, "non-"))
		}
		return true
	}
	for _, exact := range l.Models {
		if model == exact {
			return true
		}
	}
	lower := strings.ToLower(model)
	for _, family := range l.ModelFamilies {
		if strings.Contains(lower, strings.ToLower(family)) {
			return true
		}
	}
	return false
}

func cloneTime(v *time.Time) *time.Time {
	if v == nil {
		return nil
	}
	t := *v
	return &t
}
