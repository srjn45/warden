package insights

import (
	"fmt"
	"sort"
	"time"
)

// maxParallelSuggestions caps the suggestion list so the report stays scannable.
const maxParallelSuggestions = 10

// ParallelSuggestion is one "these could have run concurrently" hint: a pair of
// finished sessions in the same repo whose wall-clock windows did not overlap and
// whose touched-file sets are disjoint, with the wall-clock the overlap would
// have saved.
type ParallelSuggestion struct {
	A        string `json:"a"`
	B        string `json:"b"`
	ALabel   string `json:"a_label"`
	BLabel   string `json:"b_label"`
	Repo     string `json:"repo"`
	SavedSec int64  `json:"saved_sec"`
	Reason   string `json:"reason"`
}

// SuggestParallelization scans finished, file-attributed sessions that share a
// repo for pairs that ran sequentially (their wall-clock windows did not overlap)
// yet touched disjoint file sets — work that, with no file dependency between
// them, could have run concurrently. A pair is NOT suggested when it already
// overlapped in time (it was effectively parallel) or shares any file (a possible
// dependency or merge conflict — the conservative read is "leave it sequential").
// Disjointness is plain set membership over the recovered file lists (reusing the
// same touched-file signal collab/digest already surface), not a fresh diff.
//
// Each suggestion estimates the saving as the shorter run's duration — the span
// that could have been hidden behind the longer one. Output is deterministic:
// sorted by estimated saving (desc), then by the two session ids.
func SuggestParallelization(sessions []SessionRecord, now time.Time) []ParallelSuggestion {
	cand := make([]SessionRecord, 0, len(sessions))
	for _, s := range sessions {
		// Only finished, repo-scoped, file-attributed sessions are comparable: an
		// active session has no settled window, and a session with no recovered
		// files can't be shown disjoint from anything.
		if s.active() || s.Repo == "" || len(s.Files) == 0 {
			continue
		}
		if s.Start.IsZero() || s.End.IsZero() {
			continue
		}
		cand = append(cand, s)
	}
	sort.Slice(cand, func(i, j int) bool {
		if !cand[i].Start.Equal(cand[j].Start) {
			return cand[i].Start.Before(cand[j].Start)
		}
		return cand[i].ID < cand[j].ID
	})

	out := make([]ParallelSuggestion, 0)
	for i := 0; i < len(cand); i++ {
		for j := i + 1; j < len(cand); j++ {
			a, b := cand[i], cand[j]
			if a.Repo != b.Repo {
				continue
			}
			if windowsOverlap(a, b) {
				continue // already concurrent — nothing to suggest
			}
			if !disjointFiles(a.Files, b.Files) {
				continue // shared file ⇒ possible dependency ⇒ keep sequential
			}
			saved := minDuration(a.duration(now), b.duration(now))
			out = append(out, ParallelSuggestion{
				A:        a.ID,
				B:        b.ID,
				ALabel:   a.label(),
				BLabel:   b.label(),
				Repo:     a.Repo,
				SavedSec: int64(saved.Seconds()),
				Reason: fmt.Sprintf(
					"%s and %s ran sequentially in %s but touched disjoint files; "+
						"they had no file dependency and could run as a 2-job pipeline",
					a.label(), b.label(), a.Repo),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SavedSec != out[j].SavedSec {
			return out[i].SavedSec > out[j].SavedSec
		}
		if out[i].A != out[j].A {
			return out[i].A < out[j].A
		}
		return out[i].B < out[j].B
	})
	if len(out) > maxParallelSuggestions {
		out = out[:maxParallelSuggestions]
	}
	return out
}

// windowsOverlap reports whether two half-open [Start, End) spans intersect.
func windowsOverlap(a, b SessionRecord) bool {
	return a.Start.Before(b.End) && b.Start.Before(a.End)
}

// disjointFiles reports whether two path sets share no element.
func disjointFiles(a, b []string) bool {
	set := make(map[string]struct{}, len(a))
	for _, f := range a {
		set[f] = struct{}{}
	}
	for _, f := range b {
		if _, ok := set[f]; ok {
			return false
		}
	}
	return true
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
