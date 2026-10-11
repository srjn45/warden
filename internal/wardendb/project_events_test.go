package wardendb

import (
	"github.com/srjn45/warden/internal/audit"
	"testing"
	"time"
)

func TestProjectEventsCausalityOrderingAndAudit(t *testing.T) {
	r := &recorder{}
	l := NewProjectEventLog(ProjectEventRetention{}, r)
	got, err := l.AppendOperation("op-1", ProjectEventInput{Collection: "agents", RecordID: "a", Kind: "project.child_changed", Change: "upsert", ProjectID: "p"}, ProjectEventInput{Collection: "pipelines", RecordID: "q", Kind: "project.child_changed", Change: "status", ProjectID: "p"})
	if err != nil || len(got) != 2 {
		t.Fatalf("append = %#v, %v", got, err)
	}
	if got[0].CommitRevision != 1 || got[0].Sequence != 1 || got[1].Sequence != 2 || got[0].CausalOperationID != "op-1" {
		t.Fatalf("bad ordered causal events: %#v", got)
	}
	if got[0].EventID == got[1].EventID || got[0].AuditID == "" {
		t.Fatal("identities not allocated")
	}
	if e := r.event(ActionProjectEvent); e == nil || e.Detail["operation_id"] != "op-1" || e.Detail["audit_id"] != got[1].AuditID {
		t.Fatalf("audit correlation missing: %#v", e)
	}
}
func TestProjectEventsMonotonicAndMoveInvalidatesBoth(t *testing.T) {
	l := NewProjectEventLog(ProjectEventRetention{}, nil)
	a, _ := l.AppendOperation("op-a", ProjectEventInput{Collection: "agents", RecordID: "a", Kind: "project.child_moved", Change: "membership", PreviousProjectID: "old", ProjectID: "new"})
	b, _ := l.AppendOperation("op-b", ProjectEventInput{Collection: "agents", RecordID: "b", Kind: "project.child_changed", Change: "upsert", ProjectID: "new"})
	if a[0].CommitRevision != 1 || b[0].CommitRevision != 2 || l.Revision() != 2 {
		t.Fatalf("revisions not monotonic: %#v %#v", a, b)
	}
	if len(a[0].ProjectIDs) != 2 || a[0].ProjectIDs[0] != "new" || a[0].ProjectIDs[1] != "old" {
		t.Fatalf("move projects = %#v", a[0].ProjectIDs)
	}
	if got := l.Since(1); len(got) != 1 || got[0].EventID != b[0].EventID {
		t.Fatalf("since = %#v", got)
	}
}
func TestProjectEventRetentionPreservesRevision(t *testing.T) {
	l := NewProjectEventLog(ProjectEventRetention{MaxEvents: 1, MaxAge: time.Hour}, nil)
	l.now = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
	_, _ = l.AppendOperation("one", ProjectEventInput{Collection: "agents", RecordID: "a", Kind: "project.child_changed", Change: "upsert", ProjectID: "p"})
	l.now = func() time.Time { return time.Date(2026, 1, 1, 2, 0, 0, 0, time.UTC) }
	e, _ := l.AppendOperation("two", ProjectEventInput{Collection: "agents", RecordID: "b", Kind: "project.child_changed", Change: "upsert", ProjectID: "p"})
	got := l.Since(0)
	if l.Revision() != 2 || len(got) != 1 || got[0].EventID != e[0].EventID {
		t.Fatalf("retention = %#v revision=%d", got, l.Revision())
	}
}
func TestProjectEventRequiresCausalOperation(t *testing.T) {
	l := NewProjectEventLog(ProjectEventRetention{}, nil)
	if _, err := l.AppendOperation("", ProjectEventInput{Collection: "agents", RecordID: "a", Kind: "x", Change: "x"}); err == nil {
		t.Fatal("accepted missing causal operation")
	}
}

var _ audit.Event
