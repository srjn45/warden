package daemon

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/srjn45/warden/internal/store"
)

func (s *Server) registerHistoryRoutes(r chi.Router) {
	r.Get("/history", s.handleHistory)
}

// filterClosed narrows archived records to those updated at/after `since` (zero
// time = no lower bound) and matching `typ` (empty = any type), preserving the
// newest-first order ListClosed already guarantees. A positive limit caps the
// result. Pure: it never mutates the input slice.
func filterClosed(sessions []*store.Session, since time.Time, typ store.Type, limit int) []*store.Session {
	out := make([]*store.Session, 0, len(sessions))
	for _, s := range sessions {
		if !since.IsZero() && s.UpdatedAt.Before(since) {
			continue
		}
		if typ != "" && s.Type != typ {
			continue
		}
		out = append(out, s)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

// handleHistory browses the archived (closed/) store: GET /history with optional
// ?since=<RFC3339>, ?type=<task type>, and ?limit=<n> filters. Read-only — it
// surfaces the records the soft-delete path already persists, so `warden history`
// and the web Archive tab can review past agents.
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	var since time.Time
	if v := r.URL.Query().Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid since (want RFC3339): "+err.Error())
			return
		}
		since = t
	}
	var typ store.Type
	if v := r.URL.Query().Get("type"); v != "" {
		// Normalize so a legacy/loose type string still filters as the caller meant.
		typ = store.NormalizeType(v)
	}
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	closed, err := s.store.ListClosed(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sessionsResponse{Sessions: filterClosed(closed, since, typ, limit)})
}
