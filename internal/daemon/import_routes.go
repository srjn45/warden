package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/srjn45/warden/internal/store"
)

func (s *Server) registerImportRoutes(r chi.Router) {
	r.Post("/import", s.handleImport)
}

// handleImport ingests an export envelope (POST /import?merge=true) and inserts
// its Session records into the active store. It is metadata-only: no worktree,
// branch, or tmux session is recreated — the records simply remember where their
// (now absent) worktrees used to live. Idempotent by id: re-importing the same
// dump skips records that already exist, unless ?merge=true overwrites them.
func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	merge := r.URL.Query().Get("merge") == "true"
	var env store.Export
	if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid import body: "+err.Error())
		return
	}
	res, err := importSessions(r.Context(), s.store, &env, merge)
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// importSessions inserts each record from env into st, keyed on session id for
// idempotency. A record whose id already exists is skipped (default) or, with
// merge, deleted and re-inserted from the imported data. A brand-new id whose
// human-friendly name collides with a different existing record is imported with
// the name dropped (recorded under Renamed) so the data still lands rather than
// being lost to a cosmetic clash. It mutates env.Sessions only on that rename
// path. A malformed record (no id) aborts the whole run so a partial import is
// never silently reported as success.
func importSessions(ctx context.Context, st store.Store, env *store.Export, merge bool) (store.ImportResult, error) {
	var res store.ImportResult
	for _, sess := range env.Sessions {
		if sess == nil || sess.ID == "" {
			return res, errors.New("import contains a record with no id")
		}
		_, getErr := st.Get(ctx, sess.ID)
		exists := getErr == nil
		if getErr != nil && !errors.Is(getErr, store.ErrNotFound) {
			return res, fmt.Errorf("import %s: lookup failed: %w", sess.ID, getErr)
		}
		if exists {
			if !merge {
				res.Skipped = append(res.Skipped, sess.ID)
				continue
			}
			// Overwrite: drop the stale record so the re-insert (same id, possibly
			// same name) cannot trip the store's id/name uniqueness checks.
			if err := st.Delete(ctx, sess.ID); err != nil {
				return res, fmt.Errorf("merge %s: delete failed: %w", sess.ID, err)
			}
		}
		if err := st.Insert(ctx, sess); err != nil {
			// A name clash against a *different* active record: keep the record by
			// importing it without the colliding alias rather than failing the run.
			if errors.Is(err, store.ErrNameExists) {
				sess.Name = ""
				if err2 := st.Insert(ctx, sess); err2 == nil {
					res.Imported = append(res.Imported, sess.ID)
					res.Renamed = append(res.Renamed, sess.ID)
					continue
				}
			}
			return res, fmt.Errorf("import %s: %w", sess.ID, err)
		}
		if exists {
			res.Merged = append(res.Merged, sess.ID)
		} else {
			res.Imported = append(res.Imported, sess.ID)
		}
	}
	return res, nil
}
