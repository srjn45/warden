package wardendb

// Project events are an append-only, derived invalidation stream.  They are
// never consulted for membership or lifecycle decisions: those remain the
// normalized records validated by project_validation.go.

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/srjn45/warden/internal/audit"
)

const ActionProjectEvent = "wardendb.project.event"

var ErrInvalidProjectEvent = errors.New("wardendb: invalid project event")

// ProjectEvent is the durable-shaped event envelope from the ProjectView
// contract. CausalOperationID joins this invalidation fact to the operation
// which committed the authoritative write.
type ProjectEvent struct {
	EventID           string    `json:"event_id"`
	CausalOperationID string    `json:"causal_operation_id"`
	CommitRevision    uint64    `json:"commit_revision"`
	Sequence          uint64    `json:"sequence"`
	OccurredAt        time.Time `json:"occurred_at"`
	ProjectIDs        []string  `json:"project_ids"`
	Collection        string    `json:"collection"`
	RecordID          string    `json:"record_id"`
	Kind              string    `json:"kind"`
	Change            string    `json:"change"`
	PreviousProjectID string    `json:"previous_project_id,omitempty"`
	ProjectID         string    `json:"project_id,omitempty"`
	AuditID           string    `json:"audit_id"`
}

// ProjectEventInput is an event without identities allocated by the log.
type ProjectEventInput struct {
	ProjectIDs        []string
	Collection        string
	RecordID          string
	Kind              string
	Change            string
	PreviousProjectID string
	ProjectID         string
}

// ProjectEventRetention bounds retained (derived) history. Revisions remain
// monotonic even after pruning; consumers seeing a gap must rebuild.
type ProjectEventRetention struct {
	MaxEvents int
	MaxAge    time.Duration
}

// ProjectEventLog assigns globally monotonic commit revisions and sequence
// numbers. An operation may append multiple events in one revision; callers
// must use AppendOperation so a causal operation id is mandatory.
type ProjectEventLog struct {
	mu           sync.Mutex
	nextRevision uint64
	events       []ProjectEvent
	retention    ProjectEventRetention
	now          func() time.Time
	auditor      Auditor
}

func NewProjectEventLog(retention ProjectEventRetention, auditor Auditor) *ProjectEventLog {
	return &ProjectEventLog{retention: retention, auditor: auditor, now: time.Now}
}

// AppendOperation appends facts caused by one already-committed operation.
// The returned events are ordered by (CommitRevision, Sequence).
func (l *ProjectEventLog) AppendOperation(operationID string, in ...ProjectEventInput) ([]ProjectEvent, error) {
	if l == nil || operationID == "" {
		return nil, fmt.Errorf("%w: causal operation id", ErrInvalidProjectEvent)
	}
	if len(in) == 0 {
		return nil, fmt.Errorf("%w: no events", ErrInvalidProjectEvent)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, e := range in {
		if err := validProjectEvent(e); err != nil {
			return nil, err
		}
	}
	l.nextRevision++
	revision, at := l.nextRevision, l.now().UTC()
	out := make([]ProjectEvent, len(in))
	for i, input := range in {
		ids := normalizedProjectIDs(input.ProjectIDs, input.PreviousProjectID, input.ProjectID)
		seq := uint64(i + 1)
		e := ProjectEvent{EventID: fmt.Sprintf("pe:%014d:%02d", revision, seq), CausalOperationID: operationID, CommitRevision: revision, Sequence: seq, OccurredAt: at, ProjectIDs: ids, Collection: input.Collection, RecordID: input.RecordID, Kind: input.Kind, Change: input.Change, PreviousProjectID: input.PreviousProjectID, ProjectID: input.ProjectID, AuditID: fmt.Sprintf("project-event:%014d:%02d", revision, seq)}
		l.events = append(l.events, e)
		out[i] = e
		if l.auditor != nil {
			l.auditor.Log(audit.Event{Time: at, Action: ActionProjectEvent, Actor: "wardendb", Target: e.EventID, Detail: map[string]string{"audit_id": e.AuditID, "operation_id": operationID, "commit_revision": fmt.Sprint(revision), "sequence": fmt.Sprint(seq)}})
		}
	}
	l.prune(at)
	return out, nil
}

// Since returns retained events strictly after revision, in stream order.
func (l *ProjectEventLog) Since(revision uint64) []ProjectEvent {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]ProjectEvent, 0, len(l.events))
	for _, e := range l.events {
		if e.CommitRevision > revision {
			e.ProjectIDs = append([]string(nil), e.ProjectIDs...)
			out = append(out, e)
		}
	}
	return out
}

func (l *ProjectEventLog) Revision() uint64 {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.nextRevision
}

func (l *ProjectEventLog) prune(now time.Time) {
	start := 0
	if l.retention.MaxAge > 0 {
		cutoff := now.Add(-l.retention.MaxAge)
		for start < len(l.events) && l.events[start].OccurredAt.Before(cutoff) {
			start++
		}
	}
	if l.retention.MaxEvents > 0 && len(l.events)-start > l.retention.MaxEvents {
		start = len(l.events) - l.retention.MaxEvents
	}
	if start > 0 {
		l.events = append([]ProjectEvent(nil), l.events[start:]...)
	}
}

func validProjectEvent(e ProjectEventInput) error {
	if e.Collection == "" || e.RecordID == "" || e.Kind == "" || e.Change == "" {
		return fmt.Errorf("%w: collection, record id, kind and change are required", ErrInvalidProjectEvent)
	}
	return nil
}
func normalizedProjectIDs(ids []string, old, current string) []string {
	seen := map[string]bool{}
	for _, id := range append(append([]string(nil), ids...), old, current) {
		if id != "" {
			seen[id] = true
		}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
