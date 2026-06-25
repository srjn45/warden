// Package audit records an append-only trail of meaningful daemon actions —
// who did what, when, and to which object — in ~/.warden/audit.jsonl. Each
// action is one JSON object on its own line (JSON Lines, "jsonl"). Writes are
// best-effort: any failure to record is logged and swallowed so auditing never
// blocks or fails the action it is auditing.
package audit

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
)

// Action names for the audited daemon operations. They are part of the on-disk
// schema (callers filter on them), so existing values are stable.
const (
	ActionSpawn          = "spawn"
	ActionTerminate      = "terminate"
	ActionDelete         = "delete"
	ActionApprove        = "approve"
	ActionPipelineStart  = "pipeline_start"
	ActionPipelineCancel = "pipeline_cancel"
)

// Event is one audit record: who (Actor) did what (Action) when (Time) to which
// object (Target), with action-specific extras (Detail). The schema is append-
// only — fields are added, never renamed or removed — so old lines stay
// parseable by newer readers.
type Event struct {
	Time   time.Time         `json:"time"`             // when the action occurred
	Action string            `json:"action"`           // what was done (one of the Action* constants)
	Actor  string            `json:"actor,omitempty"`  // who initiated it (request origin; "" = unknown/local)
	Target string            `json:"target,omitempty"` // the object acted on (agent ID, pipeline ID, …)
	Detail map[string]string `json:"detail,omitempty"` // action-specific extras (name, repo, option, …)
}

// Writer appends events to a JSONL file. The zero value is unusable; build one
// with NewWriter. A nil *Writer is a valid no-op sink (auditing disabled), so
// callers need not branch on whether auditing is configured.
type Writer struct {
	mu   sync.Mutex
	path string
}

// NewWriter returns a Writer that appends to the file at path. The file (and any
// missing parent handling is the caller's) is created on first write with 0600
// perms — the trail can name agents and prompts, so keep it owner-only.
func NewWriter(path string) *Writer { return &Writer{path: path} }

// Log appends ev to the audit file. It stamps Time if unset, then writes one
// line. Best-effort by contract: it never returns an error and never panics —
// an I/O failure is logged via slog and swallowed. Safe for concurrent callers;
// the per-Writer lock keeps lines from interleaving. A nil Writer is a no-op.
func (w *Writer) Log(ev Event) {
	if w == nil {
		return
	}
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	if err := w.append(ev); err != nil {
		slog.Warn("audit: failed to record event", "action", ev.Action, "target", ev.Target, "err", err)
	}
}

func (w *Writer) append(ev Event) error {
	line, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	line = append(line, '\n')

	w.mu.Lock()
	defer w.mu.Unlock()
	f, err := os.OpenFile(w.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(line)
	return err
}

// Filter narrows a Read of the audit trail. Zero-value fields don't filter, so a
// zero Filter returns everything.
type Filter struct {
	Action string    // exact match on Event.Action
	Target string    // substring match on Event.Target
	Since  time.Time // keep events at or after this time
	Until  time.Time // keep events at or before this time
	Limit  int       // keep only the most recent N matches (0 = no cap)
}

func (f Filter) match(ev Event) bool {
	if f.Action != "" && ev.Action != f.Action {
		return false
	}
	if f.Target != "" && !strings.Contains(ev.Target, f.Target) {
		return false
	}
	if !f.Since.IsZero() && ev.Time.Before(f.Since) {
		return false
	}
	if !f.Until.IsZero() && ev.Time.After(f.Until) {
		return false
	}
	return true
}

// Read parses the JSONL audit file at path and returns matching events in file
// (chronological append) order, oldest first. A missing file yields an empty
// slice and no error. Malformed lines are skipped rather than failing the read —
// the trail is append-only and a crash can leave a final partial line. With
// Filter.Limit > 0 only the most recent Limit matches are returned.
func Read(path string, f Filter) ([]Event, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return []Event{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()

	out := []Event{}
	sc := bufio.NewScanner(file)
	// Allow long lines (a spawn detail can carry a sizable prompt-derived name).
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			continue // skip a corrupt or partially written line
		}
		if f.match(ev) {
			out = append(out, ev)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[len(out)-f.Limit:]
	}
	return out, nil
}
