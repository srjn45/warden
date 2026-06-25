package daemon

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/srjn45/warden/internal/store"
)

func (s *Server) registerSearchRoutes(r chi.Router) {
	r.Get("/search", s.handleSearch)
}

// sessionHaystack concatenates the searchable text of a session — name, id,
// ticket, type, subject, tags, prompt, branch, and the last pane excerpt —
// lowercased for case-insensitive matching. These are the fields a human scans
// to find an agent again.
func sessionHaystack(s *store.Session) string {
	var b strings.Builder
	fields := []string{
		s.Name, s.ID, s.Ticket, string(s.Type), s.Subject,
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
func sessionMatches(s *store.Session, terms []string) bool {
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
func searchSessions(sessions []*store.Session, query string) []*store.Session {
	terms := strings.Fields(strings.ToLower(query))
	out := make([]*store.Session, 0)
	for _, s := range sessions {
		if sessionMatches(s, terms) {
			out = append(out, s)
		}
	}
	return out
}

// handleSearch runs an in-memory full-text search across sessions: GET
// /search?q=<query>. By default it searches active sessions; ?closed=true folds
// in the archived (closed/) store too, so a search can reach finished agents.
// Read-only; recomputed per request. An empty/blank query returns no matches
// (400) rather than the whole fleet.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	if strings.TrimSpace(query) == "" {
		writeErr(w, http.StatusBadRequest, "empty search query")
		return
	}
	sessions, err := s.store.List(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if r.URL.Query().Get("closed") == "true" {
		closed, err := s.store.ListClosed(r.Context())
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		sessions = append(sessions, closed...)
	}
	writeJSON(w, http.StatusOK, sessionsResponse{Sessions: searchSessions(sessions, query)})
}
