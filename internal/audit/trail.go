package audit

// Trail is the audit surface shared by the file-backed Writer and the
// shared-handle implementation in internal/audit/auditstore.
type Trail interface {
	Log(ev Event)
	Read(f Filter) ([]Event, error)
}

var _ Trail = (*Writer)(nil)

// Read parses the writer's trail with f; a nil Writer (auditing off) is empty.
func (w *Writer) Read(f Filter) ([]Event, error) {
	if w == nil {
		return []Event{}, nil
	}
	return Read(w.path, f)
}

// Matches reports whether ev passes f's field filters (Limit is applied by the
// reader), so alternative Trail implementations share Read's semantics.
func (f Filter) Matches(ev Event) bool { return f.match(ev) }
