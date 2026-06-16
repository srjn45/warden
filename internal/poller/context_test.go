package poller

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/ctxtokens"
	"github.com/srjn45/warden/internal/store"
)

func TestDecideContext(t *testing.T) {
	const cool = 2 * time.Minute
	cases := []struct {
		name        string
		prev, cur   ctxtokens.State
		status      store.Status
		sinceCmpct  time.Duration
		warnAlert   bool
		autoCompact bool
		wantAlert   bool
		wantCompact bool
	}{
		{"ok->warning alerts", ctxtokens.StateOK, ctxtokens.StateWarning, store.StatusWorking, time.Hour, true, true, true, false},
		{"warning steady no alert", ctxtokens.StateWarning, ctxtokens.StateWarning, store.StatusWorking, time.Hour, true, true, false, false},
		{"warning->critical alerts, working defers compact", ctxtokens.StateWarning, ctxtokens.StateCritical, store.StatusWorking, time.Hour, true, true, true, false},
		{"critical idle compacts (deferred case, no edge)", ctxtokens.StateCritical, ctxtokens.StateCritical, store.StatusIdle, time.Hour, true, true, false, true},
		{"critical waiting compacts", ctxtokens.StateCritical, ctxtokens.StateCritical, store.StatusWaitingForInput, time.Hour, true, true, false, true},
		{"critical idle within cooldown skips compact", ctxtokens.StateCritical, ctxtokens.StateCritical, store.StatusIdle, 30 * time.Second, true, true, false, false},
		{"warnAlert off suppresses alert", ctxtokens.StateOK, ctxtokens.StateWarning, store.StatusWorking, time.Hour, false, true, false, false},
		{"autoCompact off suppresses compact", ctxtokens.StateWarning, ctxtokens.StateCritical, store.StatusIdle, time.Hour, true, false, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := decideContext(c.prev, c.cur, c.status, c.sinceCmpct, cool, c.warnAlert, c.autoCompact)
			if d.Alert != c.wantAlert || d.Compact != c.wantCompact {
				t.Fatalf("alert=%v compact=%v, want alert=%v compact=%v", d.Alert, d.Compact, c.wantAlert, c.wantCompact)
			}
		})
	}
}

type ctxFakeDeps struct {
	tokens    int
	tokensOK  bool
	updated   []string // "tokens:state"
	compacted int
	stamped   int
}

func (f *ctxFakeDeps) List(context.Context) ([]*store.Session, error) { return nil, nil }
func (f *ctxFakeDeps) UpdateStatusIf(context.Context, string, store.Status, store.Status) (bool, error) {
	return false, nil
}
func (f *ctxFakeDeps) UpdatePane(context.Context, string, string) error          { return nil }
func (f *ctxFakeDeps) UpdateSubject(context.Context, string, string) error       { return nil }
func (f *ctxFakeDeps) SessionAlive(context.Context, string) bool                 { return true }
func (f *ctxFakeDeps) CapturePane(context.Context, string) (string, error)       { return "", nil }
func (f *ctxFakeDeps) Summarize(context.Context, *store.Session) (string, error) { return "", nil }
func (f *ctxFakeDeps) ExitCode(context.Context, string) (int, bool)              { return 0, false }
func (f *ctxFakeDeps) FinalizeExit(context.Context, string, store.Status, store.Status, int) (bool, error) {
	return false, nil
}
func (f *ctxFakeDeps) ClearExit(context.Context, string) {}
func (f *ctxFakeDeps) ContextTokens(context.Context, *store.Session) (int, bool) {
	return f.tokens, f.tokensOK
}
func (f *ctxFakeDeps) UpdateContext(_ context.Context, _ string, tokens int, state string) error {
	f.updated = append(f.updated, fmt.Sprintf("%d:%s", tokens, state))
	return nil
}
func (f *ctxFakeDeps) Compact(context.Context, *store.Session) error  { f.compacted++; return nil }
func (f *ctxFakeDeps) StampCompact(context.Context, string) error     { f.stamped++; return nil }
func (f *ctxFakeDeps) SendKeys(context.Context, string, string) error { return nil }

func TestCheckContextCriticalIdleCompactsAndPersists(t *testing.T) {
	fd := &ctxFakeDeps{tokens: 420000, tokensOK: true}
	p := New(fd, time.Minute)
	p.TokenWarn, p.TokenCrit = 200000, 400000
	p.WarnAlert, p.AutoCompact, p.TokenGuard = true, true, true
	var alerts int
	p.OnContextAlert = func(*store.Session, ctxtokens.State, int) { alerts++ }

	s := &store.Session{ID: "a1", Status: store.StatusIdle, ContextState: ""}
	p.checkContext(context.Background(), s, time.Now())

	if len(fd.updated) == 0 {
		t.Fatal("gauge not persisted")
	}
	if alerts != 1 {
		t.Fatalf("alerts=%d, want 1 (\"\"→critical is a crossing)", alerts)
	}
	if fd.compacted != 1 || fd.stamped != 1 {
		t.Fatalf("compacted=%d stamped=%d, want 1/1", fd.compacted, fd.stamped)
	}
}

func TestCheckContextNoUsageIsNoop(t *testing.T) {
	fd := &ctxFakeDeps{tokensOK: false}
	p := New(fd, time.Minute)
	p.TokenGuard, p.AutoCompact, p.WarnAlert = true, true, true
	p.checkContext(context.Background(), &store.Session{ID: "a1", Status: store.StatusIdle}, time.Now())
	if len(fd.updated) != 0 || fd.compacted != 0 {
		t.Fatal("no-usage read must be a no-op")
	}
}
