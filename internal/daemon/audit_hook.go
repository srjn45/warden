package daemon

import (
	"net/http"

	"github.com/srjn45/warden/internal/audit"
)

// SetAudit wires the append-only audit writer. A nil writer (the default) leaves
// auditing off: every recordAudit call is then a no-op, so handlers don't branch.
func (s *Server) SetAudit(w *audit.Writer) { s.audit = w }

// recordAudit appends one best-effort audit event for action on target, stamping
// the caller's origin (who) from the request. Detail may be nil. It never blocks
// or fails the handler — a nil writer is a no-op and write errors are swallowed.
func (s *Server) recordAudit(r *http.Request, action, target string, detail map[string]string) {
	s.audit.Log(audit.Event{
		Action: action,
		Actor:  clientIP(r),
		Target: target,
		Detail: detail,
	})
}
