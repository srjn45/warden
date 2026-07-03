package curate

import (
	"regexp"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/memory"
)

// Result reports what a merge/age-out/staleness pass changed, so the curator can
// decide whether to write (Changed) and log a compact summary. It is the audit
// trail the never-auto-commit invariant leans on: every count here is a working-tree
// edit a human still has to approve in the committed diff.
type Result struct {
	Added      int // brand-new proposals appended (unverified)
	Superseded int // older entries struck by a contradicting proposal (tombstoned)
	Promoted   int // unverified entries corroborated by a second sighting → trusted
	AgedOut    int // stale-by-TTL unverified entries tombstoned
	Stale      int // entries whose named path no longer exists, flagged
}

// Changed reports whether the pass mutated the memory at all.
func (r Result) Changed() bool {
	return r.Added+r.Superseded+r.Promoted+r.AgedOut+r.Stale > 0
}

// valueTokenRe masks the "value" tokens of a fact — a `backtick command`, any token
// bearing a path separator, or a dotted file.ext — so two facts about the SAME
// subject that name DIFFERENT locations/commands collide on their topic key (a
// contradiction to supersede) while genuinely unrelated facts never do.
var valueTokenRe = regexp.MustCompile("`[^`]+`|\\S*/\\S*|\\b[\\w-]+\\.[\\w.-]+\\b")

var wsRe = regexp.MustCompile(`\s+`)

// normalize lowercases, collapses whitespace, and trims surrounding punctuation so
// two textually-equivalent facts compare equal (the re-observation / dedup key).
func normalize(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = wsRe.ReplaceAllString(s, " ")
	return strings.Trim(s, " .,:;!?")
}

// topicKey is the supersession key: the normalized text with value tokens masked to
// a placeholder. "X lives in foo/" and "X lives in bar/" share a topic (contradiction
// → supersede); "config lives in a/" and "tests live in b/" do not (subjects differ);
// a fact with no value token keys on its whole normalized text, so only a near-exact
// duplicate ever collides — never a false supersession of unrelated prose.
func topicKey(text string) string {
	masked := valueTokenRe.ReplaceAllString(text, "§")
	return normalize(masked)
}

// Merge folds candidate proposals into m per the §4.2 policy and reports what changed.
// Candidates are always UNVERIFIED (verify-before-trust: a proposal is a hint, never
// authority). For each candidate, against the LIVE entries:
//   - a re-observation (same normalized text) from a DIFFERENT provenance corroborates
//     it: an unverified match is promoted to trusted; the duplicate is dropped.
//   - a same-topic-but-different-text entry is CONTRADICTED: the old entry is
//     tombstoned (struck, with a reason for the diff reviewer) and the candidate is
//     appended.
//   - anything else is a genuinely new fact and is appended.
//
// A candidate is NEVER appended already-trusted; promotion happens only by
// corroboration here (or by a human editing the committed diff).
func Merge(m *memory.Memory, candidates []memory.Entry, now time.Time) Result {
	var r Result
	date := now.Format("2006-01-02")
	for _, cand := range candidates {
		cand.Trust = memory.TrustUnverified // enforce the invariant regardless of caller
		if strings.TrimSpace(cand.Text) == "" {
			continue
		}
		ntext := normalize(cand.Text)
		tkey := topicKey(cand.Text)

		matched := false
		for i := range m.Entries {
			e := &m.Entries[i]
			if !e.Live() {
				continue
			}
			if normalize(e.Text) == ntext {
				// Re-observation. Corroborate only across a DIFFERENT provenance — a
				// single agent re-asserting its own belief is not a second witness.
				if e.Trust == memory.TrustUnverified && e.Provenance != cand.Provenance {
					e.Trust = memory.TrustTrusted
					if cand.Provenance != "" {
						e.Provenance = e.Provenance + " · corroborated by " + cand.Provenance
					}
					r.Promoted++
				}
				matched = true
				break
			}
			if topicKey(e.Text) == tkey {
				// Contradiction: the newer proposal supersedes the older entry.
				e.Tombstone = true
				e.Note = "superseded " + date
				if cand.Provenance != "" {
					e.Note += " by " + cand.Provenance
				}
				m.Entries = append(m.Entries, cand)
				r.Superseded++
				r.Added++
				matched = true
				break
			}
		}
		if !matched {
			m.Entries = append(m.Entries, cand)
			r.Added++
		}
	}
	return r
}

// AgeOut tombstones LIVE, unverified entries whose absolute timestamp is older than
// ttl and were not re-corroborated (still unverified): an un-recorroborated hint that
// has sat unproven past its TTL is demoted so stale facts do not accrete (§4.2).
// Trusted and plain human-authored entries never age out — corroboration/authorship
// is the signal that they still matter. Entries with no parseable timestamp are left
// untouched (no basis to age them).
func (r *Result) AgeOut(m *memory.Memory, ttl time.Duration, now time.Time) {
	if ttl <= 0 {
		return
	}
	cutoff := now.Add(-ttl)
	date := now.Format("2006-01-02")
	for i := range m.Entries {
		e := &m.Entries[i]
		if !e.Live() || e.Trust != memory.TrustUnverified || e.Timestamp == "" {
			continue
		}
		ts, err := time.Parse("2006-01-02", e.Timestamp)
		if err != nil {
			continue
		}
		if ts.Before(cutoff) {
			e.Tombstone = true
			e.Note = "aged out " + date + " (unverified past TTL)"
			r.AgedOut++
		}
	}
}
