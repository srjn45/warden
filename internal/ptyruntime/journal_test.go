package ptyruntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/ptyevent"
)

// synth appends synthetic events the way the session writer would.
type synth struct {
	t    *testing.T
	j    *Journal
	mono int64
}

func (s *synth) add(k ptyevent.Kind, fill func(*ptyevent.Event)) []ptyevent.Event {
	s.t.Helper()
	s.mono++
	ev := ptyevent.Event{V: ptyevent.SchemaVersion, SessionID: s.j.SessionID(), Epoch: 1, Seq: s.j.NextSeq(),
		MonoNS: s.mono, Wall: time.Now(), Kind: k, Redacted: true}
	fill(&ev)
	out, err := s.j.Append(ev)
	if err != nil {
		s.t.Fatal(err)
	}
	return out
}

func (s *synth) output(n int) {
	s.add(ptyevent.KindOutput, func(e *ptyevent.Event) {
		e.Output = &ptyevent.Output{Data: []byte(strings.Repeat("o", n))}
	})
}

func (s *synth) snapshot() []ptyevent.Event {
	return s.add(ptyevent.KindMarker, func(e *ptyevent.Event) {
		e.Marker = &ptyevent.Marker{Type: ptyevent.MarkerSnapshot, ToSeq: e.Seq - 1, Snapshot: "screen", Cols: 80, Rows: 24}
	})
}

func newSynth(t *testing.T, ret Retention) *synth {
	t.Helper()
	j, err := OpenJournal(filepath.Join(t.TempDir(), "j"), "s", ret)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = j.Close() })
	s := &synth{t: t, j: j}
	s.add(ptyevent.KindLifecycle, func(e *ptyevent.Event) {
		e.Lifecycle = &ptyevent.Lifecycle{Phase: ptyevent.PhaseSpawned, PID: 1}
	})
	return s
}

func TestJournalRejectsContractViolations(t *testing.T) {
	s := newSynth(t, Retention{})
	base := func() ptyevent.Event {
		return ptyevent.Event{V: 1, SessionID: "s", Epoch: 1, Seq: s.j.NextSeq(), MonoNS: 100, Kind: ptyevent.KindOutput,
			Output: &ptyevent.Output{Data: []byte("x")}, Redacted: true}
	}
	cases := map[string]func(*ptyevent.Event){
		"unredacted":    func(e *ptyevent.Event) { e.Redacted = false },
		"seq hole":      func(e *ptyevent.Event) { e.Seq += 2 },
		"seq duplicate": func(e *ptyevent.Event) { e.Seq-- },
		"other session": func(e *ptyevent.Event) { e.SessionID = "x" },
		"bad epoch":     func(e *ptyevent.Event) { e.Epoch = 2 }, // a new epoch must start with spawned/recovered
		"bad span": func(e *ptyevent.Event) {
			e.Redactions = []ptyevent.Redaction{{Kind: ptyevent.SecretJWT, Offset: 0, Length: 9}}
		},
	}
	for name, mut := range cases {
		ev := base()
		mut(&ev)
		if _, err := s.j.Append(ev); err == nil {
			t.Errorf("%s: appended", name)
		}
	}
	// After a terminal lifecycle no I/O until the next epoch.
	code := 0
	s.add(ptyevent.KindLifecycle, func(e *ptyevent.Event) {
		e.Lifecycle = &ptyevent.Lifecycle{Phase: ptyevent.PhaseExited, ExitCode: &code}
	})
	s.add(ptyevent.KindMarker, func(e *ptyevent.Event) { e.Marker = &ptyevent.Marker{Type: ptyevent.MarkerReset} })
	if _, err := s.j.Append(base()); !errors.Is(err, ErrOrder) {
		t.Fatalf("output after exit: %v", err)
	}
	if s.j.Last().Seq != 3 {
		t.Fatalf("rejected events advanced the journal to %d", s.j.Last().Seq)
	}
}

func TestJournalRetentionTrimsWholeSnapshotSegments(t *testing.T) {
	s := newSynth(t, Retention{MaxBytes: 4 << 10, KeepSnapshots: 3})
	var trims []ptyevent.Event
	for i := 0; i < 12; i++ {
		for k := 0; k < 4; k++ {
			s.output(200)
		}
		for _, ev := range s.snapshot() {
			if ev.Kind == ptyevent.KindMarker && ev.Marker.Type == ptyevent.MarkerTrim {
				trims = append(trims, ev)
			}
		}
	}
	if len(trims) == 0 {
		t.Fatal("no trim marker")
	}
	w := s.j.Window()
	if w.OldestSeq <= 1 || s.j.Bytes() > 4<<10+2048 {
		t.Fatalf("not trimmed: window %+v bytes %d", w, s.j.Bytes())
	}
	// The oldest retained event is a snapshot that covers everything before it.
	var first ptyevent.Event
	_ = s.j.Read(w.OldestSeq, func(ev ptyevent.Event) error { first = ev; return errStop })
	if first.Kind != ptyevent.KindMarker || first.Marker.Type != ptyevent.MarkerSnapshot || first.Marker.ToSeq+1 != w.OldestSeq {
		t.Fatalf("oldest retained event = %+v", first)
	}
	last := trims[len(trims)-1].Marker
	if last.ToSeq+1 != w.OldestSeq {
		t.Fatalf("trim marker %+v vs oldest %d", last, w.OldestSeq)
	}
	segs, _ := filepath.Glob(filepath.Join(s.j.dir, segPrefix+"*"))
	if len(segs) > 4 {
		t.Fatalf("%d segments retained, want ≤ KeepSnapshots+1", len(segs))
	}
	if err := s.j.Read(1, func(ptyevent.Event) error { return nil }); !errors.Is(err, ErrTrimmed) {
		t.Fatalf("read below retention: %v", err)
	}
	// A cursor that fell out of retention resumes from the snapshot.
	plan := ptyevent.PlanResume(ptyevent.Cursor{SessionID: "s", Epoch: 1, Seq: 2}, w)
	if plan.Mode != ptyevent.ResumeSnapshot || plan.FromSeq != w.SnapshotSeq+1 {
		t.Fatalf("plan = %+v (window %+v)", plan, w)
	}
	// The retained journal still passes the stream contract, and survives reopen.
	var evs []ptyevent.Event
	_ = s.j.Read(w.OldestSeq, func(ev ptyevent.Event) error { evs = append(evs, ev); return nil })
	if vs := ptyevent.ValidateStream(evs); len(vs) > 0 {
		t.Fatal(vs)
	}
	_ = s.j.Close()
	j2, err := OpenJournal(s.j.dir, "s", Retention{})
	if err != nil {
		t.Fatal(err)
	}
	defer j2.Close()
	if j2.Window() != w || j2.Recovery() != (Recovery{}) {
		t.Fatalf("reopen window %+v recovery %+v, want %+v", j2.Window(), j2.Recovery(), w)
	}
}

func TestJournalRecoveryDropsCorruptMiddle(t *testing.T) {
	s := newSynth(t, Retention{})
	s.output(10)
	s.snapshot()
	s.output(10)
	s.snapshot()
	s.output(10)
	_ = s.j.Close()
	segs, _ := filepath.Glob(filepath.Join(s.j.dir, segPrefix+"*"))
	if len(segs) != 3 {
		t.Fatalf("segments = %d", len(segs))
	}
	if err := os.WriteFile(segs[1], []byte("not json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	j, err := OpenJournal(s.j.dir, "s", Retention{})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if rec := j.Recovery(); rec.DroppedSegments != 1 || rec.TruncatedBytes == 0 {
		t.Fatalf("recovery = %+v", rec)
	}
	if j.Last().Seq != 2 || j.NextSeq() != 3 {
		t.Fatalf("head after recovery = %d", j.Last().Seq)
	}
}

func TestJournalExpiry(t *testing.T) {
	s := newSynth(t, Retention{MaxAge: time.Hour})
	if s.j.Expired(time.Now().Add(2 * time.Hour)) {
		t.Fatal("a live journal never expires")
	}
	code := 0
	s.add(ptyevent.KindLifecycle, func(e *ptyevent.Event) {
		e.Lifecycle = &ptyevent.Lifecycle{Phase: ptyevent.PhaseExited, ExitCode: &code}
	})
	if s.j.Expired(time.Now()) || !s.j.Expired(time.Now().Add(2*time.Hour)) {
		t.Fatal("expiry after MaxAge")
	}
	if err := s.j.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.j.dir); !os.IsNotExist(err) {
		t.Fatal("journal dir not removed")
	}
}

func TestScreenModel(t *testing.T) {
	sc := NewScreen(10, 3)
	_, _ = sc.Write([]byte("hello\r\nwor"))
	_, _ = sc.Write([]byte("ld\x1b[1;1HH\x1b[2;3H\x1b[K"))
	if got := sc.String(); got != "Hello\nwo" {
		t.Fatalf("screen = %q", got)
	}
	_, _ = sc.Write([]byte("\x1b[?1049hALT\xe4\xb8"))
	_, _ = sc.Write([]byte("\x96"))
	if got := sc.String(); got != "\n  ALT世" { // the cursor keeps its position on the alt screen
		t.Fatalf("alt screen = %q", got)
	}
	_, _ = sc.Write([]byte("\x1b[?1049l"))
	if got := sc.String(); got != "Hello\nwo" {
		t.Fatalf("after alt exit = %q", got)
	}
	_, _ = sc.Write([]byte("\x1b[3;1H0123456789AB\nC"))
	if got := sc.String(); got != "0123456789\nAB\n  C" { // autowrap + LF scroll the top lines away
		t.Fatalf("wrap/scroll = %q", got)
	}
	sc.Resize(4, 2)
	if c, r := sc.Size(); c != 4 || r != 2 {
		t.Fatalf("size = %dx%d", c, r)
	}
	_, _ = sc.Write([]byte("\x1b[2J\x1b]0;t\x07\x1bPdcs\x1b\\x"))
	if got := strings.TrimSpace(sc.String()); got != "x" {
		t.Fatalf("after clear = %q", got)
	}
}

func TestKeyBytes(t *testing.T) {
	cases := []struct {
		keys []string
		want string
	}{
		{[]string{"claude --resume x", "Enter"}, "claude --resume x\r"},
		{[]string{"C-c"}, "\x03"},
		{[]string{"Escape", "Up", "Down"}, "\x1b\x1b[A\x1b[B"},
		{[]string{"M-Enter"}, "\x1b\r"},
		{[]string{"M-x"}, "\x1bx"},
		{[]string{"-l", "Enter"}, "Enter"},
		{[]string{"2"}, "2"},
		{[]string{"BTab", "BSpace"}, "\x1b[Z\x7f"},
	}
	for _, c := range cases {
		if got := string(KeyBytes(c.keys...)); got != c.want {
			t.Errorf("KeyBytes(%q) = %q, want %q", c.keys, got, c.want)
		}
	}
}

func TestHostImplementsTmuxprocHost(t *testing.T) {
	noLeaks(t)
	exe := fakeArgv(t)[0]
	t.Setenv(fakeEnv, "repl")
	h := &Host{JournalDir: t.TempDir(), Shell: []string{exe}, Options: Options{KillGrace: 300 * time.Millisecond}}
	defer h.Close()
	ctx := context.Background()
	if err := h.NewSession(ctx, "agent-1", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if err := h.NewSession(ctx, "agent-1", t.TempDir()); err == nil {
		t.Fatal("duplicate live session accepted")
	}
	if !h.HasSession(ctx, "agent-1") || h.HasSession(ctx, "nope") {
		t.Fatal("HasSession")
	}
	if err := h.SendKeys(ctx, "agent-1", "say", " via-host", "Enter"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(waitFor)
	for {
		pane, err := h.CapturePane(ctx, "agent-1")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(pane, "\nvia-host") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pane = %q", pane)
		}
		time.Sleep(5 * time.Millisecond)
	}
	ins := 0
	_ = h.Session("agent-1").Journal().Read(1, func(ev ptyevent.Event) error {
		if ev.Kind == ptyevent.KindInput && ev.Input.Origin == ptyevent.OriginOperator {
			ins++
		}
		return nil
	})
	if ins != 1 {
		t.Fatalf("operator inputs journaled = %d", ins)
	}
	if err := h.KillSession(ctx, "agent-1"); err != nil {
		t.Fatal(err)
	}
	if h.HasSession(ctx, "agent-1") || h.KillSession(ctx, "agent-1") != nil {
		t.Fatal("killed session still present / second kill errored")
	}
	if _, err := h.CapturePane(ctx, "agent-1"); err == nil {
		t.Fatal("capture of a missing session")
	}
	if _, err := os.Stat(filepath.Join(h.JournalDir, "agent-1")); err != nil {
		t.Fatal("journal must outlive KillSession")
	}
}
