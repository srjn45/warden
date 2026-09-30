package daemon

import (
	"context"
	"errors"
	"fmt"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/store"
)

type importStore interface {
	Get(ctx context.Context, id string) (*agentstore.Agent, error)
	Delete(ctx context.Context, id string) error
	Insert(ctx context.Context, agent *agentstore.Agent) error
}

// importSessions inserts each record from env into st, keyed on session id for
// idempotency. A record whose id already exists is skipped (default) or, with
// merge, deleted and re-inserted from the imported data. A brand-new id whose
// human-friendly name collides with a different existing record is imported with
// the name dropped (recorded under Renamed) so the data still lands rather than
// being lost to a cosmetic clash. It mutates env.Sessions only on that rename
// path. A malformed record (no id) aborts the whole run so a partial import is
// never silently reported as success.
func importSessions(ctx context.Context, st importStore, env *store.Export, merge bool) (store.ImportResult, error) {
	var res store.ImportResult
	for _, sess := range env.Sessions {
		if sess == nil || sess.ID == "" {
			return res, errors.New("import contains a record with no id")
		}
		if sess.IsTerminal() {
			res.Skipped = append(res.Skipped, sess.ID)
			continue
		}
		_, getErr := st.Get(ctx, sess.ID)
		exists := getErr == nil
		if getErr != nil && !errors.Is(getErr, agentstore.ErrNotFound) {
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
		agent := agentstore.FromSession(sess)
		if err := st.Insert(ctx, agent); err != nil {
			// A name clash against a *different* active record: keep the record by
			// importing it without the colliding alias rather than failing the run.
			if errors.Is(err, agentstore.ErrNameExists) {
				sess.Name = ""
				agent.Name = ""
				if err2 := st.Insert(ctx, agent); err2 == nil {
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
