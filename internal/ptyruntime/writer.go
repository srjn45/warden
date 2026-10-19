package ptyruntime

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/srjn45/warden/internal/ptyevent"
)

// writer is the journaling core shared by a direct Session and a tmux shadow
// Mirror: the single per-session writer that owns the seq, runs the contract
// pipeline (SplitUTF8 → Redactor → ControlScanner), feeds the Screen model,
// appends to the Journal and only then fans events out.
type writer struct {
	opt   Options
	j     *Journal
	id    string
	epoch uint64
	start time.Time
	hub   *hub

	mu         sync.Mutex // writer state below (and the embedding type's)
	in         ingest
	screen     *Screen
	closed     bool
	sinceSnap  int
	pendingGap bool
	journalErr error

	closing chan struct{} // closed once the owner is fully torn down
	wg      sync.WaitGroup
}

func newWriter(opt Options, j *Journal, cols, rows uint16) writer {
	return writer{opt: opt, j: j, id: j.SessionID(), epoch: j.Last().Epoch + 1, start: time.Now(), hub: newHub(),
		screen: NewScreen(int(cols), int(rows)), closing: make(chan struct{})}
}

// ID is the session id.
func (s *writer) ID() string { return s.id }

// Epoch is this runtime incarnation's epoch.
func (s *writer) Epoch() uint64 { return s.epoch }

// Journal is the session's journal.
func (s *writer) Journal() *Journal { return s.j }

// Screen renders the current (redacted) screen model.
func (s *writer) Screen() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.screen.String()
}

// JournalErr is the last journal write failure (nil when healthy). The events
// lost to it are covered by a gap marker on the next successful write.
func (s *writer) JournalErr() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.journalErr
}

// --- events ---------------------------------------------------------------

// event builds an event header for the current epoch. Caller holds mu.
func (s *writer) event(k ptyevent.Kind) ptyevent.Event {
	return ptyevent.Event{V: ptyevent.SchemaVersion, SessionID: s.id, Epoch: s.epoch, Seq: s.j.NextSeq(),
		MonoNS: time.Since(s.start).Nanoseconds(), Wall: time.Now(), Kind: k, Redacted: true}
}

// commit validates, journals and fans out ev. A journal I/O failure is
// remembered and turned into a gap marker on the next write; a contract
// violation is returned without side effects. Caller holds mu.
func (s *writer) commit(ev ptyevent.Event) error {
	if err := ev.Validate(); err != nil {
		return err
	}
	if s.pendingGap && !(ev.Kind == ptyevent.KindMarker && ev.Marker.Type == ptyevent.MarkerGap) {
		if err := s.gap(); err != nil {
			return err
		}
		ev.Seq = s.j.NextSeq()
		if ev.Epoch == s.epoch {
			ev.MonoNS = time.Since(s.start).Nanoseconds() // stay after the gap/snapshot markers
		}
	}
	evs, err := s.j.Append(ev)
	for _, e := range evs {
		s.hub.publish(e)
	}
	if err != nil {
		s.journalErr = err
		s.pendingGap = true
		return err
	}
	return nil
}

// gap journals marker{gap} for a hole (dropped output or lost events) and a
// fresh snapshot, and resets the parser that the hole desynchronized.
func (s *writer) gap() error {
	s.pendingGap = false
	n := s.j.NextSeq()
	ev := s.event(ptyevent.KindMarker)
	ev.Marker = &ptyevent.Marker{Type: ptyevent.MarkerGap, FromSeq: n + 1, ToSeq: n + 1}
	if err := s.commit(ev); err != nil {
		return err
	}
	s.in.reset()
	return s.snapshot()
}

// snapshot journals marker{snapshot} of the screen model. Caller holds mu.
func (s *writer) snapshot() error {
	ev := s.event(ptyevent.KindMarker)
	cols, rows := s.screen.Size()
	text, _ := ptyevent.Redact([]byte(s.screen.String())) // defense in depth; fed redacted bytes already
	ev.Marker = &ptyevent.Marker{Type: ptyevent.MarkerSnapshot, ToSeq: ev.Seq - 1, Snapshot: string(text),
		Cols: uint16(cols), Rows: uint16(rows)}
	s.sinceSnap = 0
	return s.commit(ev)
}

func (s *writer) commitOutput(out ptyevent.Output, reds []ptyevent.Redaction) {
	ev := s.event(ptyevent.KindOutput)
	ev.Output, ev.Redactions = &out, reds
	_, _ = s.screen.Write(out.Data)
	if s.commit(ev) != nil {
		return
	}
	s.sinceSnap += len(out.Data)
	altExit := false
	for _, c := range out.Controls {
		altExit = altExit || c.Kind == ptyevent.ControlAltScreenExit
	}
	if altExit || s.sinceSnap >= s.opt.SnapshotEvery {
		_ = s.snapshot()
	}
}

func (s *writer) flushOutput() {
	if out, reds, ok := s.in.flush(); ok {
		s.commitOutput(out, reds)
	}
}

// Emit journals an event produced above the runtime — a committed state
// change (t3), a prompt observation (t3/t6), a decision (t4) or a
// ready/attached/detached lifecycle step — and returns its seq. The runtime
// fills the header (session, epoch, seq, clocks) and redacts prompt evidence;
// output/input/resize/markers and process lifecycle are runtime-owned.
func (s *writer) Emit(ev ptyevent.Event) (uint64, error) {
	switch ev.Kind {
	case ptyevent.KindState, ptyevent.KindDecision:
	case ptyevent.KindPrompt:
		if ev.Prompt != nil && ev.Prompt.Prompt != nil {
			p := *ev.Prompt.Prompt
			raw, reds := ptyevent.Redact([]byte(p.Evidence.Raw))
			if len(reds) > 0 {
				p.Evidence.Raw, p.Evidence.Redactions = string(raw), append(p.Evidence.Redactions, reds...)
			}
			pe := *ev.Prompt
			pe.Prompt = &p
			ev.Prompt = &pe
		}
	case ptyevent.KindLifecycle:
		if ev.Lifecycle == nil || (ev.Lifecycle.Phase != ptyevent.PhaseReady &&
			ev.Lifecycle.Phase != ptyevent.PhaseAttached && ev.Lifecycle.Phase != ptyevent.PhaseDetached) {
			return 0, fmt.Errorf("%w: lifecycle %v", ErrEmitKind, ev.Lifecycle)
		}
	default:
		return 0, fmt.Errorf("%w: %s", ErrEmitKind, ev.Kind)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, ErrClosed
	}
	h := s.event(ev.Kind)
	ev.V, ev.SessionID, ev.Epoch, ev.Seq, ev.MonoNS, ev.Wall, ev.Redacted = h.V, h.SessionID, h.Epoch, h.Seq, h.MonoNS, h.Wall, true
	if err := s.commit(ev); err != nil {
		return 0, err
	}
	return s.j.Last().Seq, nil
}

// --- fan-out ---------------------------------------------------------------

// Subscribe resolves cursor with ptyevent.PlanResume and streams events from
// there: journal replay, then the live feed, handed off without loss (the live
// feed is registered before the journal head is read; overlap is dropped). A
// zero cursor is a fresh attach. On ResumeReject, C is closed and Err is
// ErrRejected.
func (s *writer) Subscribe(c ptyevent.Cursor) *Subscription {
	out := make(chan ptyevent.Event)
	sub := &Subscription{C: out, live: make(chan ptyevent.Event, s.opt.SubscriberBuffer), done: make(chan struct{}), h: s.hub}
	s.mu.Lock()
	w := s.j.Window()
	sub.Plan, sub.Head = ptyevent.PlanResume(c, w), w.NewestSeq
	ok := sub.Plan.Mode != ptyevent.ResumeReject && !s.closed && s.hub.addSub(sub)
	if ok {
		// Under mu, before Close can mark closed and Wait: Add never races Wait.
		s.wg.Add(1)
	}
	s.mu.Unlock()
	if !ok {
		if sub.Plan.Mode == ptyevent.ResumeReject {
			sub.fail(ErrRejected)
		} else {
			sub.fail(ErrClosed)
		}
		close(out)
		return sub
	}
	go s.pump(sub, out, w.NewestSeq)
	return sub
}

var errStop = errors.New("stop")

func (s *writer) pump(sub *Subscription, out chan<- ptyevent.Event, head uint64) {
	defer s.wg.Done()
	defer close(out)
	send := func(ev ptyevent.Event) bool {
		select {
		case out <- ev:
			return true
		case <-sub.done:
		case <-s.closing:
			sub.fail(ErrClosed)
		}
		return false
	}
	last := head
	if sub.Plan.Mode != ptyevent.ResumeTail {
		last = sub.Plan.FromSeq - 1
		err := s.j.Read(sub.Plan.FromSeq, func(ev ptyevent.Event) error {
			if ev.Seq > head {
				return errStop
			}
			if !send(ev) {
				return errStop
			}
			last = ev.Seq
			return nil
		})
		if errors.Is(err, ErrTrimmed) {
			sub.fail(ErrResync)
			return
		}
		if err != nil && !errors.Is(err, errStop) {
			sub.fail(err)
			return
		}
		if last < head {
			return // stopped by Close
		}
	}
	for {
		select {
		case ev, ok := <-sub.live:
			if !ok {
				if sub.lagged.Load() {
					sub.fail(ErrResync)
				} else {
					sub.fail(ErrClosed)
				}
				return
			}
			if ev.Seq <= last {
				continue // replay overlap
			}
			if !send(ev) {
				return
			}
			last = ev.Seq
		case <-sub.done:
			return
		case <-s.closing:
			sub.fail(ErrClosed)
			return
		}
	}
}
