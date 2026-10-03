package daemon

import (
	"strings"

	"github.com/srjn45/warden/internal/agentstore"
)

// sessionHaystack concatenates the searchable text of a session — name, id,
// ticket, role, type (deprecated), subject, tags, prompt, branch, and the last
// pane excerpt — lowercased for case-insensitive matching. These are the fields
// a human scans to find an agent again.
func sessionHaystack(s *agentstore.Agent) string {
	var b strings.Builder
	fields := []string{
		s.Name, s.ID, s.Ticket, s.Role, string(s.Type), s.Subject,
		s.Prompt, s.Branch, s.LastPaneExcerpt,
	}
	fields = append(fields, s.Tags...) // tags are searchable like any other label
	for _, f := range fields {
		b.WriteString(f)
		b.WriteByte('\n')
	}
	return strings.ToLower(b.String())
}

// sessionMatches reports whether s matches the query. The query is split on
// whitespace into terms; every term must appear somewhere in the session's
// haystack (AND semantics), so "review auth" finds an agent whose subject
// mentions auth and whose type is pr-review. An all-blank query matches nothing.
func sessionMatches(s *agentstore.Agent, terms []string) bool {
	if len(terms) == 0 {
		return false
	}
	hay := sessionHaystack(s)
	for _, t := range terms {
		if !strings.Contains(hay, t) {
			return false
		}
	}
	return true
}

// searchSessions returns every session whose text matches the query. Order is
// preserved from the input (the daemon hands it newest-first). Pure.
func searchSessions(sessions []*agentstore.Agent, query string) []*agentstore.Agent {
	terms := strings.Fields(strings.ToLower(query))
	out := make([]*agentstore.Agent, 0)
	for _, s := range sessions {
		if sessionMatches(s, terms) {
			out = append(out, s)
		}
	}
	return out
}
