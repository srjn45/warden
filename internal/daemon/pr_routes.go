package daemon

import (
	"strings"

	"github.com/srjn45/warden/internal/digest"
	"github.com/srjn45/warden/internal/store"
)

// prTitle picks a human PR title for an agent: its live one-line subject, else
// the digest's parsed task (the first user prompt, truncated), else the branch.
func prTitle(sess *store.Session, d digest.Digest) string {
	for _, c := range []string{sess.Subject, d.Task} {
		if t := truncateTitle(strings.TrimSpace(c)); t != "" {
			return t
		}
	}
	if d.Branch != "" {
		return d.Branch
	}
	return sess.ID
}

// truncateTitle collapses a candidate to its first line and caps it at 72 chars
// (with an ellipsis) so a long prompt does not become an unwieldy PR title.
func truncateTitle(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	const max = 72
	if len(s) > max {
		return strings.TrimSpace(s[:max-1]) + "…"
	}
	return s
}
