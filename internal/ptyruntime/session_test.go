package ptyruntime

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"

	"github.com/srjn45/warden/internal/ptyevent"
)

const waitFor = 10 * time.Second

// noLeaks fails the test if any runtime goroutine outlives it.
func noLeaks(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		deadline := time.Now().Add(3 * time.Second)
		for {
			buf := make([]byte, 1<<20)
			stacks := string(buf[:runtime.Stack(buf, true)])
			var leaked []string
			for _, g := range strings.Split(stacks, "\n\n") {
				if strings.Contains(g, "ptyruntime.(*Session)") || strings.Contains(g, "ptyruntime.(*writer)") ||
					strings.Contains(g, "ptyruntime.(*Mirror)") || strings.Contains(g, "ptyruntime.(*hub)") {
					leaked = append(leaked, g)
				}
			}
			if len(leaked) == 0 {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%d leaked goroutines:\n%s", len(leaked), strings.Join(leaked, "\n\n"))
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
}

type rig struct {
	t *testing.T
	j *Journal
	s *Session
}

func startFake(t *testing.T, opt Options) *rig {
	t.Helper()
	noLeaks(t)
	j, err := OpenJournal(filepath.Join(t.TempDir(), "j"), "sess-1", Retention{})
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, j: j}
	r.start(opt)
	t.Cleanup(func() { _ = r.s.Close(); _ = j.Close() })
	return r
}

func (r *rig) start(opt Options) {
	r.t.Helper()
	if opt.KillGrace == 0 {
		opt.KillGrace = 500 * time.Millisecond
	}
	if opt.DrainGrace == 0 {
		opt.DrainGrace = 500 * time.Millisecond
	}
	s, err := Start(Config{Journal: r.j, Argv: fakeArgv(r.t), Env: fakeEnviron("repl"), Cols: 80, Rows: 24,
		Backend: "fake", Options: opt})
	if err != nil {
		r.t.Fatal(err)
	}
	r.s = s
}

// human types a line as an attached human client.
func (r *rig) human(line string) {
	r.t.Helper()
	arb, err := r.s.Input(Write{Origin: ptyevent.OriginHuman, ClientID: "c1", Data: []byte(line + "\n")})
	if err != nil || arb.Verdict != ptyevent.VerdictWrite {
		r.t.Fatalf("human input %q: %v %+v", line, err, arb)
	}
}

// events returns the whole journal, checked against the stream contract.
func (r *rig) events() []ptyevent.Event {
	r.t.Helper()
	var evs []ptyevent.Event
	if err := r.j.Read(1, func(ev ptyevent.Event) error { evs = append(evs, ev); return nil }); err != nil {
		r.t.Fatal(err)
	}
	if vs := ptyevent.ValidateStream(evs); len(vs) > 0 {
		r.t.Fatalf("journal violates the stream contract: %v", vs)
	}
	return evs
}

// waitOutput polls the journal until its redacted output contains want.
func (r *rig) waitOutput(want string) string {
	r.t.Helper()
	return r.waitOutputN(want, 1)
}

// waitOutputN waits until want occurs n times in the journal's output.
func (r *rig) waitOutputN(want string, n int) string {
	r.t.Helper()
	deadline := time.Now().Add(waitFor)
	for {
		var b strings.Builder
		if err := r.j.Read(1, func(ev ptyevent.Event) error {
			if ev.Kind == ptyevent.KindOutput {
				b.Write(ev.Output.Data)
			}
			return nil
		}); err != nil {
			r.t.Fatalf("read journal: %v", err)
		}
		if strings.Count(b.String(), want) >= n {
			return b.String()
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("timed out waiting for %q in output (head %d, journal err %v):\n%q", want, r.j.Last().Seq, r.s.JournalErr(), b.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (r *rig) waitDone() {
	r.t.Helper()
	select {
	case <-r.s.Done():
	case <-time.After(waitFor):
		r.t.Fatal("process exit was never journaled")
	}
}

func kinds(evs []ptyevent.Event, k ptyevent.Kind) []ptyevent.Event {
	var out []ptyevent.Event
	for _, ev := range evs {
		if ev.Kind == k {
			out = append(out, ev)
		}
	}
	return out
}

func markers(evs []ptyevent.Event, mt ptyevent.MarkerType) []ptyevent.Event {
	var out []ptyevent.Event
	for _, ev := range kinds(evs, ptyevent.KindMarker) {
		if ev.Marker.Type == mt {
			out = append(out, ev)
		}
	}
	return out
}

func TestSessionLifecycleAndExit(t *testing.T) {
	r := startFake(t, Options{})
	r.waitOutput("ready")
	r.human("say hello world")
	r.waitOutput("hello world\r\n")
	r.human("exit 3")
	r.waitDone()
	if ex := r.s.Exit(); ex.Phase != ptyevent.PhaseExited || ex.ExitCode == nil || *ex.ExitCode != 3 {
		t.Fatalf("exit = %+v", ex)
	}
	evs := r.events()
	first, last := evs[0], evs[len(evs)-1]
	if first.Kind != ptyevent.KindLifecycle || first.Lifecycle.Phase != ptyevent.PhaseSpawned || first.Seq != 1 || first.Epoch != 1 {
		t.Fatalf("first event = %+v", first)
	}
	if last.Kind != ptyevent.KindLifecycle || last.Lifecycle.Phase != ptyevent.PhaseExited {
		t.Fatalf("last event = %+v", last)
	}
	ins := kinds(evs, ptyevent.KindInput)
	if len(ins) != 2 || string(ins[0].Input.Data) != "say hello world\n" || ins[0].Input.Origin != ptyevent.OriginHuman {
		t.Fatalf("inputs = %+v", ins)
	}
	if _, err := r.s.Input(Write{Origin: ptyevent.OriginHuman, Data: []byte("x")}); !errors.Is(err, ErrExited) {
		t.Fatalf("input after exit: %v", err)
	}
	if r.s.Screen() == "" || !strings.Contains(r.s.Screen(), "hello world") {
		t.Fatalf("screen = %q", r.s.Screen())
	}
}

func TestSignaledExit(t *testing.T) {
	r := startFake(t, Options{})
	r.waitOutput("ready")
	r.human("kill")
	r.waitDone()
	if ex := r.s.Exit(); ex.Phase != ptyevent.PhaseSignaled || ex.Signal != "SIGKILL" {
		t.Fatalf("exit = %+v", ex)
	}
	r.events()
}

func TestPartialUTF8AcrossReads(t *testing.T) {
	r := startFake(t, Options{})
	r.waitOutput("ready")
	r.human("utf8")
	all := r.waitOutput("🙂")
	if !strings.Contains(all, "héllo→世界🙂") {
		t.Fatalf("output lost or mangled runes: %q", all)
	}
	for _, ev := range kinds(r.events(), ptyevent.KindOutput) {
		if !ev.Output.Partial && !utf8.Valid(ev.Output.Data) {
			t.Fatalf("event %d cuts a rune: %q", ev.Seq, ev.Output.Data)
		}
	}
}

func TestIdleFlushReleasesPartialRune(t *testing.T) {
	r := startFake(t, Options{IdleFlush: 20 * time.Millisecond})
	r.waitOutput("ready")
	r.human("half")
	r.waitOutput("cut\xe4\xb8")
	var partial bool
	for _, ev := range kinds(r.events(), ptyevent.KindOutput) {
		partial = partial || (ev.Output.Partial && bytes.HasSuffix(ev.Output.Data, []byte("\xe4\xb8")))
	}
	if !partial {
		t.Fatal("held-back incomplete rune was not flushed as a Partial output")
	}
}

func TestControlSequencesAndAltScreenSnapshot(t *testing.T) {
	r := startFake(t, Options{})
	r.waitOutput("ready")
	r.human("controls")
	r.waitOutput("main")
	evs := r.events()
	seen := map[ptyevent.ControlKind]string{}
	for _, ev := range kinds(evs, ptyevent.KindOutput) {
		for _, c := range ev.Output.Controls {
			seen[c.Kind] = c.Param
		}
	}
	for _, k := range []ptyevent.ControlKind{ptyevent.ControlAltScreenEnter, ptyevent.ControlAltScreenExit,
		ptyevent.ControlTitle, ptyevent.ControlClearScreen, ptyevent.ControlCursorHide, ptyevent.ControlCursorShow} {
		if _, ok := seen[k]; !ok {
			t.Errorf("control %s not reported (saw %v)", k, seen)
		}
	}
	if seen[ptyevent.ControlTitle] != "fake title" {
		t.Errorf("title = %q", seen[ptyevent.ControlTitle])
	}
	snaps := markers(evs, ptyevent.MarkerSnapshot)
	if len(snaps) == 0 {
		t.Fatal("alt-screen exit did not take a snapshot")
	}
	if s := snaps[0].Marker.Snapshot; strings.Contains(s, "ALT") || !strings.Contains(s, "controls") {
		t.Fatalf("snapshot should show the main screen, got %q", s)
	}
}

func TestResize(t *testing.T) {
	r := startFake(t, Options{})
	r.waitOutput("ready")
	if err := r.s.Resize(0, 10, ptyevent.OriginHuman, "c1"); err == nil {
		t.Fatal("zero resize accepted")
	}
	if err := r.s.Resize(120, 40, ptyevent.OriginHuman, "c1"); err != nil {
		t.Fatal(err)
	}
	r.human("size")
	r.waitOutput("size=120x40")
	rs := kinds(r.events(), ptyevent.KindResize)
	if len(rs) != 1 || rs[0].Resize.Cols != 120 || rs[0].Resize.Rows != 40 || rs[0].Resize.ClientID != "c1" {
		t.Fatalf("resize events = %+v", rs)
	}
}

func collect(t *testing.T, sub *Subscription, until func(ptyevent.Event) bool) []ptyevent.Event {
	t.Helper()
	var evs []ptyevent.Event
	timeout := time.After(waitFor)
	for {
		select {
		case ev, ok := <-sub.C:
			if !ok {
				return evs
			}
			evs = append(evs, ev)
			if until(ev) {
				return evs
			}
		case <-timeout:
			t.Fatalf("subscription stalled after %d events", len(evs))
		}
	}
}

func outputHas(s string) func(ptyevent.Event) bool {
	return func(ev ptyevent.Event) bool {
		return ev.Kind == ptyevent.KindOutput && strings.Contains(string(ev.Output.Data), s)
	}
}

func replHas(s string) func(ptyevent.Event) bool {
	return func(ev ptyevent.Event) bool {
		if ev.Kind != ptyevent.KindOutput {
			return false
		}
		data := string(ev.Output.Data)
		return data == s || strings.Contains(data, "\n"+s)
	}
}

func TestReplayFromCursorThenLive(t *testing.T) {
	r := startFake(t, Options{})
	r.waitOutput("ready")
	r.human("say one")
	r.waitOutput("one\r\n")
	head := r.j.Last().Seq
	mid := head / 2

	// Replay from a cursor, then hand off to live without loss or duplicates.
	sub := r.s.Subscribe(ptyevent.Cursor{SessionID: "sess-1", Epoch: 1, Seq: mid})
	defer sub.Close()
	if sub.Plan.Mode != ptyevent.ResumeReplay || sub.Plan.FromSeq != mid+1 {
		t.Fatalf("plan = %+v", sub.Plan)
	}
	r.human("say two")
	got := collect(t, sub, replHas("two\r\n"))
	if got[0].Seq != mid+1 {
		t.Fatalf("replay starts at %d, want %d", got[0].Seq, mid+1)
	}
	for i := 1; i < len(got); i++ {
		if ptyevent.CheckNext(got[i-1], got[i]) != ptyevent.OrderNext {
			t.Fatalf("stream not contiguous at %d→%d", got[i-1].Seq, got[i].Seq)
		}
	}

	// Cursor at head: tail only.
	tail := r.s.Subscribe(ptyevent.Cursor{SessionID: "sess-1", Epoch: 1, Seq: r.j.Last().Seq})
	defer tail.Close()
	if tail.Plan.Mode != ptyevent.ResumeTail {
		t.Fatalf("plan = %+v", tail.Plan)
	}
	next := r.j.NextSeq()
	r.human("say three")
	if got := collect(t, tail, replHas("three\r\n")); got[0].Seq != next {
		t.Fatalf("tail starts at %d, want %d", got[0].Seq, next)
	}

	// Fresh attach without a snapshot replays from the oldest seq.
	fresh := r.s.Subscribe(ptyevent.Cursor{})
	defer fresh.Close()
	if got := collect(t, fresh, outputHas("three")); fresh.Plan.Mode != ptyevent.ResumeReplay || got[0].Seq != 1 {
		t.Fatalf("fresh plan %+v first %d", fresh.Plan, got[0].Seq)
	}

	for _, c := range []ptyevent.Cursor{{SessionID: "other", Seq: 1}, {SessionID: "sess-1", Seq: 1 << 40}} {
		rej := r.s.Subscribe(c)
		if _, ok := <-rej.C; ok || !errors.Is(rej.Err(), ErrRejected) || rej.Plan.Mode != ptyevent.ResumeReject {
			t.Fatalf("cursor %+v not rejected: %+v %v", c, rej.Plan, rej.Err())
		}
	}
}

func TestSlowSubscriberNeverBlocksThePTY(t *testing.T) {
	r := startFake(t, Options{SubscriberBuffer: 4, RawBuffer: 2})
	r.waitOutput("ready")
	slow := r.s.Subscribe(ptyevent.Cursor{SessionID: "sess-1", Epoch: 1, Seq: r.j.Last().Seq})
	defer slow.Close()
	rawSlow, err := r.s.Attach("slow-client")
	if err != nil {
		t.Fatal(err)
	}
	fast := r.s.Subscribe(ptyevent.Cursor{SessionID: "sess-1", Epoch: 1, Seq: r.j.Last().Seq})
	defer fast.Close()
	done := make(chan []ptyevent.Event)
	go func() { done <- collect(t, fast, outputHas("flood-done")) }()

	r.human("flood 2000") // ~200 KB; neither slow consumer reads
	select {
	case <-done:
	case <-time.After(waitFor):
		t.Fatal("PTY stalled behind slow subscribers")
	}
	r.waitOutput("flood-done")
	for range slow.C {
	}
	if !errors.Is(slow.Err(), ErrResync) {
		t.Fatalf("slow subscriber err = %v, want ErrResync", slow.Err())
	}
	for range rawSlow.C {
	}
	if !rawSlow.Lagged() {
		t.Fatal("slow raw client not marked lagged")
	}
	rawSlow.Close()
	dets := 0
	for _, ev := range kinds(r.events(), ptyevent.KindLifecycle) {
		if ev.Lifecycle.Phase == ptyevent.PhaseDetached && ev.Lifecycle.ClientID == "slow-client" && ev.Lifecycle.Detail != "" {
			dets++
		}
	}
	if dets != 1 {
		t.Fatalf("lagged detach journaled %d times", dets)
	}

	// The cut-off subscriber resumes from its cursor via the journal.
	resumed := r.s.Subscribe(ptyevent.Cursor{SessionID: "sess-1", Epoch: 1, Seq: 2})
	defer resumed.Close()
	collect(t, resumed, outputHas("flood-done"))
}

func TestWriterBackpressureJournalsGap(t *testing.T) {
	release := make(chan struct{})
	stalled := make(chan struct{})
	var once sync.Once
	var opt Options
	opt.QueueDepth = 1
	opt.ReadBuffer = 512
	opt.beforeOutput = func() {
		once.Do(func() { // stall the writer exactly once
			close(stalled)
			<-release
		})
	}
	r := startFake(t, opt)
	r.human("flood 3000")
	<-stalled
	time.Sleep(300 * time.Millisecond) // the reader keeps draining the PTY and drops
	close(release)
	// The loss is reported even though the PTY may have gone quiet since.
	deadline := time.Now().Add(waitFor)
	for len(markers(r.events(), ptyevent.MarkerGap)) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("dropped output was not journaled as a gap")
		}
		time.Sleep(5 * time.Millisecond)
	}
	r.human("say after-gap")
	r.waitOutput("after-gap\r\n")
	evs := r.events() // passes ValidateStream: the hole is covered by the gap marker
	for i, ev := range evs {
		if ev.Kind == ptyevent.KindMarker && ev.Marker.Type == ptyevent.MarkerGap {
			nxt := evs[i+1]
			if nxt.Seq != ev.Marker.ToSeq+1 || nxt.Kind != ptyevent.KindMarker || nxt.Marker.Type != ptyevent.MarkerSnapshot {
				t.Fatalf("gap not followed by a forced snapshot: %+v", nxt)
			}
		}
	}
}

func TestSecretsNeverReachTheJournal(t *testing.T) {
	r := startFake(t, Options{})
	r.waitOutput("ready")
	raw, err := r.s.Attach("me")
	if err != nil {
		t.Fatal(err)
	}
	var live bytes.Buffer
	liveDone := make(chan struct{})
	go func() {
		defer close(liveDone)
		for b := range raw.C {
			live.Write(b)
		}
	}()

	// Output secret: redacted in the journal, intact on the live terminal.
	r.human("secret")
	r.waitOutput("[REDACTED:anthropic_key] ok")

	// getpass (canonical, echo off): the typed password is journaled as a length.
	r.human("getpass")
	r.waitOutput("Password: ")
	const pw = "correct-horse-battery-staple"
	r.human(pw)
	r.waitOutput("got 28")

	// A credential prompt on screen makes even raw-mode typing secret.
	r.s.SetCredentialPrompt(true)
	paste := "paste-" + fakeSecret()
	r.human("say " + paste)
	r.s.SetCredentialPrompt(false)
	r.waitOutput("[REDACTED:anthropic_key]\r\n") // the CLI echoes it; output is redacted
	r.human("exit 0")
	r.waitDone()
	raw.Close()
	<-liveDone
	if !strings.Contains(live.String(), fakeSecret()) {
		t.Fatal("the live attach stream must stay unredacted")
	}

	var secretIns int
	for _, ev := range kinds(r.events(), ptyevent.KindInput) {
		if ev.Input.Secret {
			secretIns++
			if len(ev.Input.Data) != 0 || ev.Input.SecretLen == 0 {
				t.Fatalf("secret input carries data: %+v", ev.Input)
			}
		}
	}
	if secretIns != 2 {
		t.Fatalf("secret inputs = %d, want 2 (getpass + credential prompt)", secretIns)
	}
	disk := readDir(t, r.j.dir)
	for _, leak := range []string{fakeSecret(), pw} {
		if strings.Contains(disk, leak) {
			t.Fatalf("journal on disk contains %q", leak)
		}
	}
}

func readDir(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		b.Write(data)
	}
	return b.String()
}

func TestInputOwnership(t *testing.T) {
	r := startFake(t, Options{Ownership: ptyevent.OwnershipConfig{HumanQuiet: 50 * time.Millisecond, MaxDefer: 150 * time.Millisecond}})
	r.waitOutput("ready")

	if arb, _ := r.s.Input(Write{Origin: ptyevent.OriginWarden, Data: []byte("x")}); arb.Verdict != ptyevent.VerdictReject {
		t.Fatalf("warden input without decision/reason: %+v", arb)
	}
	// A human mid-line holds the lease: warden input defers, then is abandoned.
	if arb, _ := r.s.Input(Write{Origin: ptyevent.OriginHuman, ClientID: "c1", Data: []byte("say half")}); arb.Verdict != ptyevent.VerdictWrite {
		t.Fatal(arb)
	}
	humanSeq := r.s.Lease().HumanSeq
	var humanEv ptyevent.Event
	_ = r.j.Read(humanSeq, func(ev ptyevent.Event) error { humanEv = ev; return errStop })
	if humanEv.Kind != ptyevent.KindInput || string(humanEv.Input.Data) != "say half" {
		t.Fatalf("lease HumanSeq %d names %+v, want the human input", humanSeq, humanEv)
	}
	if arb, _ := r.s.Input(Write{Origin: ptyevent.OriginWarden, DecisionID: "d1", Data: []byte("1")}); arb.Verdict != ptyevent.VerdictDefer {
		t.Fatalf("warden input over a typing human: %+v", arb)
	}
	ctx, cancel := context.WithTimeout(context.Background(), waitFor)
	defer cancel()
	if err := r.s.WriteWait(ctx, Write{Origin: ptyevent.OriginWarden, DecisionID: "d1", Data: []byte("1")}); !errors.Is(err, ErrAbandoned) {
		t.Fatalf("deferred warden write: %v, want ErrAbandoned", err)
	}
	// The human submits the line; after HumanQuiet automatic input flows.
	r.human("")
	r.waitOutput("half\r\n")
	if err := r.s.WriteWait(ctx, Write{Origin: ptyevent.OriginOperator, Data: []byte("say op\n")}); err != nil {
		t.Fatal(err)
	}
	r.waitOutput("op\r\n")
	var origins []ptyevent.Origin
	for _, ev := range kinds(r.events(), ptyevent.KindInput) {
		origins = append(origins, ev.Input.Origin)
	}
	want := []ptyevent.Origin{ptyevent.OriginHuman, ptyevent.OriginHuman, ptyevent.OriginOperator}
	if len(origins) != len(want) {
		t.Fatalf("journaled inputs %v, want %v (deferred/abandoned writes must not be journaled)", origins, want)
	}
	if l := r.s.Lease(); l.Holder == ptyevent.OriginOperator {
		t.Fatal("operator lease not released after the write")
	}
}

func TestEmit(t *testing.T) {
	r := startFake(t, Options{})
	r.waitOutput("ready")
	seq, err := r.s.Emit(ptyevent.Event{Kind: ptyevent.KindState, State: &ptyevent.StateChange{
		From: ptyevent.StateUnknown, To: ptyevent.StateIdle, Evidence: []ptyevent.EvidenceKind{ptyevent.EvReady}}})
	if err != nil || seq != r.j.Last().Seq {
		t.Fatalf("emit state: seq %d err %v", seq, err)
	}
	for _, ev := range []ptyevent.Event{
		{Kind: ptyevent.KindOutput, Output: &ptyevent.Output{Data: []byte("x")}},
		{Kind: ptyevent.KindMarker, Marker: &ptyevent.Marker{Type: ptyevent.MarkerReset}},
		{Kind: ptyevent.KindLifecycle, Lifecycle: &ptyevent.Lifecycle{Phase: ptyevent.PhaseExited}},
	} {
		if _, err := r.s.Emit(ev); !errors.Is(err, ErrEmitKind) {
			t.Fatalf("emit %s: %v, want ErrEmitKind", ev.Kind, err)
		}
	}
	if _, err := r.s.Emit(ptyevent.Event{Kind: ptyevent.KindState, State: &ptyevent.StateChange{
		From: ptyevent.StateUnknown, To: ptyevent.StateIdle}}); err == nil {
		t.Fatal("state change without evidence accepted")
	}
	r.events()
}

func TestTeardownKillsProcessTree(t *testing.T) {
	r := startFake(t, Options{KillGrace: 200 * time.Millisecond})
	r.waitOutput("ready")
	r.human("stubborn") // ignores SIGTERM/SIGHUP: needs the SIGKILL escalation
	r.waitOutput("stubborn\r\n")
	r.human("child")
	r.waitOutput("child ")
	if !r.s.Alive() {
		t.Fatal("session not alive")
	}
	start := time.Now()
	if err := r.s.Close(); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("Close took %v", time.Since(start))
	}
	if r.s.Alive() {
		t.Fatal("process tree still alive after Close")
	}
	if err := unix.Kill(-r.s.PID(), 0); !errors.Is(err, unix.ESRCH) {
		t.Fatalf("process group still has members: %v", err)
	}
	select {
	case <-r.s.Done():
	default:
		t.Fatal("exit not journaled by Close")
	}
	if ex := r.s.Exit(); ex.Phase != ptyevent.PhaseSignaled || ex.Signal != "SIGKILL" {
		t.Fatalf("exit = %+v", ex)
	}
	_ = r.s.Close() // idempotent
	r.events()
}

func TestRespawnContinuesJournalWithNewEpoch(t *testing.T) {
	r := startFake(t, Options{})
	r.waitOutput("ready")
	r.human("exit 0")
	r.waitDone()
	_ = r.s.Close()
	r.start(Options{})
	r.waitOutputN("ready\r\n", 2) // second incarnation
	evs := r.events()
	if r.s.Epoch() != 2 {
		t.Fatalf("epoch = %d", r.s.Epoch())
	}
	if len(markers(evs, ptyevent.MarkerReset)) != 1 || len(markers(evs, ptyevent.MarkerSnapshot)) == 0 {
		t.Fatal("respawn must journal reset + snapshot")
	}
}

func TestCrashRecoveryTornTail(t *testing.T) {
	noLeaks(t)
	dir := filepath.Join(t.TempDir(), "j")
	j, err := OpenJournal(dir, "sess-1", Retention{})
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, j: j}
	r.start(Options{})
	r.waitOutput("ready")
	r.human("say before-crash")
	r.waitOutput("before-crash\r\n")
	// Simulate the daemon dying mid-write: a copy of the journal taken while
	// the process runs has no exit event, and its last record is torn.
	crash := filepath.Join(t.TempDir(), "crash")
	if err := os.CopyFS(crash, os.DirFS(dir)); err != nil {
		t.Fatal(err)
	}
	_ = r.s.Close()
	_ = j.Close()
	dir = crash
	evsBefore := len(kinds(readAll(t, dir), ptyevent.KindOutput))
	seg := lastSegment(t, dir)
	f, err := os.OpenFile(seg, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"v":1,"session_id":"sess-1","epoch":1,"seq":9999,"kind":"outp`)
	_ = f.Close()

	j2, err := OpenJournal(dir, "sess-1", Retention{})
	if err != nil {
		t.Fatal(err)
	}
	defer j2.Close()
	if rec := j2.Recovery(); rec.TruncatedBytes == 0 {
		t.Fatalf("torn tail not truncated: %+v", rec)
	}
	if got := len(kinds(readAll(t, dir), ptyevent.KindOutput)); got != evsBefore {
		t.Fatalf("recovery lost intact events: %d → %d", evsBefore, got)
	}
	r2 := &rig{t: t, j: j2}
	r2.start(Options{})
	defer r2.s.Close()
	r2.waitOutputN("ready\r\n", 2)
	evs := r2.events()
	var crashed bool
	for _, ev := range kinds(evs, ptyevent.KindLifecycle) {
		crashed = crashed || (ev.Lifecycle.Phase == ptyevent.PhaseCrashed && ev.Epoch == 1)
	}
	if !crashed {
		t.Fatal("lost epoch was not closed with lifecycle{crashed}")
	}
	if plan := ptyevent.PlanResume(ptyevent.Cursor{SessionID: "sess-1", Epoch: 1, Seq: 3}, j2.Window()); plan.Mode != ptyevent.ResumeSnapshot {
		t.Fatalf("pre-crash cursor plan = %+v, want snapshot (behind reset)", plan)
	}
}

func readAll(t *testing.T, dir string) []ptyevent.Event {
	t.Helper()
	j, err := OpenJournal(dir, "sess-1", Retention{})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	var evs []ptyevent.Event
	if err := j.Read(1, func(ev ptyevent.Event) error { evs = append(evs, ev); return nil }); err != nil {
		t.Fatal(err)
	}
	return evs
}

func lastSegment(t *testing.T, dir string) string {
	t.Helper()
	names, _ := filepath.Glob(filepath.Join(dir, segPrefix+"*"))
	if len(names) == 0 {
		t.Fatal("no segments")
	}
	return names[len(names)-1]
}
