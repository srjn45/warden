package ptyevent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"
)

// stream builds a journal slice with auto-assigned seq/mono, mimicking the
// runtime's single per-session writer.
type stream struct {
	evs   []Event
	seq   uint64
	epoch uint64
	mono  int64
}

func newStream() *stream { return &stream{epoch: 1} }

func (s *stream) add(kind Kind, fill func(*Event)) *stream {
	s.seq++
	s.mono += int64(10 * time.Millisecond)
	e := Event{V: SchemaVersion, SessionID: "s-1", Epoch: s.epoch, Seq: s.seq, MonoNS: s.mono,
		Wall: t0.Add(time.Duration(s.mono)), Kind: kind, Redacted: true}
	fill(&e)
	s.evs = append(s.evs, e)
	return s
}

func (s *stream) newEpoch() *stream { s.epoch++; s.mono = 0; return s }

func (s *stream) life(ph LifecyclePhase) *stream {
	return s.add(KindLifecycle, func(e *Event) {
		e.Lifecycle = &Lifecycle{Phase: ph, PID: 4242, Backend: "claude", Presentation: PresentationDirect}
		if ph == PhaseExited {
			code := 0
			e.Lifecycle.ExitCode = &code
		}
	})
}

func (s *stream) out(data string) *stream {
	return s.add(KindOutput, func(e *Event) {
		var sc ControlScanner
		_, ctl := sc.Feed([]byte(data))
		e.Output = &Output{Data: []byte(data), Controls: ctl}
	})
}

func (s *stream) in(o Origin, data string) *stream {
	return s.add(KindInput, func(e *Event) { e.Input = &Input{Data: []byte(data), Origin: o, ClientID: "web-1"} })
}

// streamFixtures are the canonical journals. Each is written to
// testdata/streams/<name>.jsonl; want lists the stream rules it must violate.
func streamFixtures() map[string]struct {
	evs  []Event
	want []string
} {
	ps := fixturePrompts()
	ordered := newStream().life(PhaseSpawned).
		out("\x1b[?1049h\x1b[2J\x1b]0;claude\a").
		life(PhaseReady).
		add(KindResize, func(e *Event) { e.Resize = &Resize{Cols: 120, Rows: 40, ClientID: "web-1", Origin: OriginHuman} }).
		in(OriginHuman, "fix the tests\r").
		out("✻ Working… (esc to interrupt)").
		add(KindState, func(e *Event) {
			e.State = &StateChange{From: StateIdle, To: StateWorking, Evidence: []EvidenceKind{EvWorkingMarker}}
		}).
		out("Bash(go test ./...)\nDo you want to proceed?\n❯ 1. Yes").
		add(KindPrompt, func(e *Event) { e.Prompt = &PromptEvent{Phase: PromptObserved, Prompt: ps["claude_bash_test"]} }).
		add(KindDecision, func(e *Event) {
			e.Decision = &DecisionEvent{ID: "d-1", Fingerprint: ps["claude_bash_test"].Fingerprint, Posture: PostureAllowlisted,
				Action: ActionApprove, OptionIndex: 1, Rule: RulePermitted, DecidedAtSeq: 9, Revalidation: RevalOK}
		}).
		add(KindInput, func(e *Event) { e.Input = &Input{Data: []byte("1"), Origin: OriginWarden, DecisionID: "d-1"} }).
		add(KindPrompt, func(e *Event) {
			e.Prompt = &PromptEvent{Phase: PromptCleared, Fingerprint: ps["claude_bash_test"].Fingerprint}
		}).
		add(KindInput, func(e *Event) { e.Input = &Input{Origin: OriginHuman, Secret: true, SecretLen: 12} }).
		life(PhaseExited)

	crash := newStream().life(PhaseSpawned).out("hello").life(PhaseCrashed).
		newEpoch().life(PhaseRecovered).
		add(KindMarker, func(e *Event) { e.Marker = &Marker{Type: MarkerReset} }).
		add(KindMarker, func(e *Event) {
			e.Marker = &Marker{Type: MarkerSnapshot, ToSeq: 5, Snapshot: "hello", Cols: 80, Rows: 24}
		}).
		out("world")

	gapMarked := newStream().life(PhaseSpawned).out("a")
	gapMarked.add(KindMarker, func(e *Event) { e.Marker = &Marker{Type: MarkerGap, FromSeq: 4, ToSeq: 6} })
	gapMarked.seq = 6
	gapMarked.out("b")

	gapUnmarked := newStream().life(PhaseSpawned).out("a")
	gapUnmarked.seq += 2
	gapUnmarked.out("b")

	gapWrong := newStream().life(PhaseSpawned).out("a")
	gapWrong.add(KindMarker, func(e *Event) { e.Marker = &Marker{Type: MarkerGap, FromSeq: 4, ToSeq: 5} })
	gapWrong.seq = 7 // hole is 4..7, marker only covers 4..5
	gapWrong.out("b")

	dup := newStream().life(PhaseSpawned).out("a")
	dup.seq--
	dup.out("a-again")

	badEpoch := newStream().life(PhaseSpawned).out("a").newEpoch().out("b")

	afterExit := newStream().life(PhaseSpawned).life(PhaseExited).out("ghost")

	unredacted := newStream().life(PhaseSpawned).out("token")
	unredacted.evs[1].Redacted = false

	monoBack := newStream().life(PhaseSpawned).out("a")
	monoBack.mono -= int64(15 * time.Millisecond)
	monoBack.out("b")

	return map[string]struct {
		evs  []Event
		want []string
	}{
		"ordered":           {ordered.evs, nil},
		"crash_recovery":    {crash.evs, nil},
		"gap_marked":        {gapMarked.evs, nil},
		"gap_unmarked":      {gapUnmarked.evs, []string{RuleSeqContiguous}},
		"gap_marker_short":  {gapWrong.evs, []string{RuleSeqContiguous}},
		"duplicate_seq":     {dup.evs, []string{RuleSeqDuplicate}},
		"epoch_bad_start":   {badEpoch.evs, []string{RuleEpochStart}},
		"output_after_exit": {afterExit.evs, []string{RuleAfterExit}},
		"unredacted":        {unredacted.evs, []string{RuleEventInvalid}},
		"mono_regression":   {monoBack.evs, []string{RuleMonoMonotonic}},
	}
}

func encodeJSONL(t *testing.T, evs []Event) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, e := range evs {
		if err := enc.Encode(e); err != nil {
			t.Fatal(err)
		}
	}
	return buf.Bytes()
}

func decodeJSONL(t *testing.T, path string) []Event {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var evs []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var e Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		evs = append(evs, e)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return evs
}

func ruleSet(vs []Violation) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range vs {
		if !seen[v.Rule] {
			seen[v.Rule] = true
			out = append(out, v.Rule)
		}
	}
	sort.Strings(out)
	return out
}

// TestStreamFixtures pins the journal wire format (golden JSONL) and the
// ordering contract: the decoded fixture must violate exactly the expected
// rules — sequence gaps, duplicates, epoch starts after a crash, monotonic
// clock, no I/O after exit, and the redaction boundary.
func TestStreamFixtures(t *testing.T) {
	fx := streamFixtures()
	names := make([]string, 0, len(fx))
	for n := range fx {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		c := fx[name]
		t.Run(name, func(t *testing.T) {
			golden(t, filepath.Join("streams", name+".jsonl"), encodeJSONL(t, c.evs))
			got := decodeJSONL(t, filepath.Join("testdata", "streams", name+".jsonl"))
			if !reflect.DeepEqual(ruleSet(ValidateStream(got)), c.want) {
				t.Fatalf("violations = %v, want %v", ValidateStream(got), c.want)
			}
		})
	}
}

func TestEventValidate(t *testing.T) {
	base := func() Event {
		return Event{V: SchemaVersion, SessionID: "s", Seq: 1, Kind: KindOutput, Output: &Output{Data: []byte("hi")}, Redacted: true}
	}
	cases := []struct {
		name string
		mut  func(*Event)
		want error
	}{
		{"ok", func(*Event) {}, nil},
		{"version", func(e *Event) { e.V = 2 }, ErrVersion},
		{"session", func(e *Event) { e.SessionID = "" }, ErrSession},
		{"seq zero", func(e *Event) { e.Seq = 0 }, ErrSeq},
		{"negative mono", func(e *Event) { e.MonoNS = -1 }, ErrMonotonic},
		{"unknown kind", func(e *Event) { e.Kind = "beep" }, ErrKind},
		{"payload mismatch", func(e *Event) { e.Kind = KindInput }, ErrPayload},
		{"two payloads", func(e *Event) { e.Resize = &Resize{Cols: 1, Rows: 1, Origin: OriginHuman} }, ErrPayload},
		{"not redacted", func(e *Event) { e.Redacted = false }, ErrNotRedacted},
		{"redaction out of range", func(e *Event) { e.Redactions = []Redaction{{Kind: SecretJWT, Offset: 1, Length: 5}} }, ErrBadRedaction},
		{"zero resize", func(e *Event) {
			e.Kind, e.Output, e.Resize = KindResize, nil, &Resize{Cols: 0, Rows: 24, Origin: OriginHuman}
		}, ErrResize},
		{"input origin", func(e *Event) { e.Kind, e.Output, e.Input = KindInput, nil, &Input{Data: []byte("x"), Origin: "ghost"} }, ErrOrigin},
		{"secret with data", func(e *Event) {
			e.Kind, e.Output, e.Input = KindInput, nil, &Input{Data: []byte("hunter2"), Origin: OriginHuman, Secret: true}
		}, ErrSecretData},
		{"phase", func(e *Event) { e.Kind, e.Output, e.Lifecycle = KindLifecycle, nil, &Lifecycle{Phase: "nap"} }, ErrPhase},
		{"state without evidence", func(e *Event) {
			e.Kind, e.Output, e.State = KindState, nil, &StateChange{From: StateIdle, To: StateWorking}
		}, ErrPayload},
		{"observed prompt without prompt", func(e *Event) {
			e.Kind, e.Output, e.Prompt = KindPrompt, nil, &PromptEvent{Phase: PromptObserved}
		}, ErrPayload},
		{"snapshot exceeds cap", func(e *Event) {
			e.Kind, e.Output, e.Marker = KindMarker, nil, &Marker{Type: MarkerSnapshot, Snapshot: string(make([]byte, 1<<20+1))}
		}, ErrSnapshotTooLarge},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := base()
			c.mut(&e)
			err := e.Validate()
			if c.want == nil && err != nil || c.want != nil && !errors.Is(err, c.want) {
				t.Fatalf("Validate() = %v, want %v", err, c.want)
			}
		})
	}
}

func TestCheckNext(t *testing.T) {
	ev := func(sess string, epoch, seq uint64) Event { return Event{SessionID: sess, Epoch: epoch, Seq: seq} }
	cases := []struct {
		name       string
		prev, next Event
		want       Order
	}{
		{"first event", Event{}, ev("s", 1, 1), OrderNext},
		{"first event missing seq 1", Event{}, ev("s", 1, 3), OrderGap},
		{"next", ev("s", 1, 4), ev("s", 1, 5), OrderNext},
		{"replay overlap after reconnect", ev("s", 1, 9), ev("s", 1, 7), OrderDuplicate},
		{"same seq again", ev("s", 1, 9), ev("s", 1, 9), OrderDuplicate},
		{"gap", ev("s", 1, 9), ev("s", 1, 12), OrderGap},
		{"new epoch continues seq", ev("s", 1, 9), ev("s", 2, 10), OrderNext},
		{"epoch regression", ev("s", 2, 9), ev("s", 1, 10), OrderRegression},
		{"foreign session", ev("s", 1, 9), ev("t", 1, 10), OrderForeign},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := CheckNext(c.prev, c.next); got != c.want {
				t.Fatalf("CheckNext = %s, want %s", got, c.want)
			}
		})
	}
}

func TestPlanResume(t *testing.T) {
	w := Window{SessionID: "s", Epoch: 2, OldestSeq: 100, NewestSeq: 500, SnapshotSeq: 450}
	cases := []struct {
		name string
		c    Cursor
		w    Window
		want ResumeMode
		from uint64
	}{
		{"fresh attach uses snapshot", Cursor{}, w, ResumeSnapshot, 451},
		{"fresh attach without snapshot replays all", Cursor{}, Window{SessionID: "s", OldestSeq: 1, NewestSeq: 9}, ResumeReplay, 1},
		{"up to date tails", Cursor{SessionID: "s", Epoch: 2, Seq: 500}, w, ResumeTail, 0},
		{"brief disconnect replays", Cursor{SessionID: "s", Epoch: 2, Seq: 480}, w, ResumeReplay, 481},
		{"cursor exactly at retention edge replays", Cursor{SessionID: "s", Epoch: 2, Seq: 99}, w, ResumeReplay, 100},
		{"cursor fell out of retention", Cursor{SessionID: "s", Epoch: 1, Seq: 40}, w, ResumeSnapshot, 451},
		{"cursor ahead of head after journal loss", Cursor{SessionID: "s", Epoch: 2, Seq: 900}, w, ResumeReject, 0},
		{"cursor for another session", Cursor{SessionID: "x", Seq: 10}, w, ResumeReject, 0},
		{"cursor behind a reset", Cursor{SessionID: "s", Epoch: 1, Seq: 300}, Window{SessionID: "s", Epoch: 2, OldestSeq: 100, NewestSeq: 500, SnapshotSeq: 420, ResetSeq: 410}, ResumeSnapshot, 421},
		{"out of retention and no snapshot", Cursor{SessionID: "s", Seq: 5}, Window{SessionID: "s", OldestSeq: 100, NewestSeq: 500}, ResumeReject, 0},
		{"snapshot older than retention is unusable", Cursor{}, Window{SessionID: "s", OldestSeq: 100, NewestSeq: 500, SnapshotSeq: 50}, ResumeReplay, 100},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := PlanResume(c.c, c.w)
			if p.Mode != c.want || p.FromSeq != c.from {
				t.Fatalf("PlanResume = %+v, want %s from %d", p, c.want, c.from)
			}
		})
	}
}

// TestReconnectReplayConverges simulates a client that drops mid-stream,
// reconnects with its cursor, receives an overlapping replay, and must end up
// having applied every event exactly once, in order.
func TestReconnectReplayConverges(t *testing.T) {
	evs := streamFixtures()["ordered"].evs
	var applied []uint64
	var last Event
	apply := func(e Event) {
		switch CheckNext(last, e) {
		case OrderNext:
			applied = append(applied, e.Seq)
			last = e
		case OrderDuplicate:
		default:
			t.Fatalf("unexpected order for seq %d after %d", e.Seq, last.Seq)
		}
	}
	for _, e := range evs[:6] {
		apply(e)
	}
	cur := Cursor{SessionID: last.SessionID, Epoch: last.Epoch, Seq: last.Seq}
	plan := PlanResume(cur, Window{SessionID: "s-1", Epoch: 1, OldestSeq: 1, NewestSeq: evs[len(evs)-1].Seq})
	if plan.Mode != ResumeReplay || plan.FromSeq != cur.Seq+1 {
		t.Fatalf("plan = %+v", plan)
	}
	// A sloppy server resends from 3: duplicates must be dropped silently.
	for _, e := range evs[2:] {
		apply(e)
	}
	if len(applied) != len(evs) {
		t.Fatalf("applied %d events, want %d", len(applied), len(evs))
	}
	for i, s := range applied {
		if s != uint64(i+1) {
			t.Fatalf("applied out of order: %v", applied)
		}
	}
}
