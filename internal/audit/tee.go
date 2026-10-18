package audit

// Tee records every event to two trails and reads from the first. Schema 2
// logs to the queryable warden-db collection AND keeps appending to
// audit.jsonl, which the migration contract declares an independent,
// append-only external stream (`warden audit`, the MCP audit tool and any
// operator tooling read it directly), so the file must not go stale.
type Tee struct {
	read  Trail
	other Trail
}

var _ Trail = (*Tee)(nil)

// NewTee builds a trail that logs to primary and secondary and reads primary.
func NewTee(primary, secondary Trail) *Tee { return &Tee{read: primary, other: secondary} }

// Log appends ev to both trails. Each is best-effort on its own, so one
// failing never suppresses the other.
func (t *Tee) Log(ev Event) {
	if t == nil {
		return
	}
	t.read.Log(ev)
	t.other.Log(ev)
}

// Read returns the primary trail's events.
func (t *Tee) Read(f Filter) ([]Event, error) {
	if t == nil {
		return []Event{}, nil
	}
	return t.read.Read(f)
}
