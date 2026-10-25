package ptyruntime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"

	"github.com/srjn45/warden/internal/ptyevent"
)

// Options tunes a Session. Zero fields take DefaultOptions' values.
type Options struct {
	// IdleFlush releases what the redactor/UTF-8 splitter hold back after this
	// much output silence (spec §5: ≈100ms keeps journal lag small).
	IdleFlush time.Duration
	// ReadBuffer is the PTY read size.
	ReadBuffer int
	// QueueDepth bounds raw reads waiting for the writer. When it is full the
	// reader drops output (never blocking the PTY) and the writer journals a
	// gap marker followed by a fresh snapshot.
	QueueDepth int
	// SubscriberBuffer bounds each Subscription's live queue; overflow cuts the
	// subscriber off with ErrResync.
	SubscriberBuffer int
	// RawBuffer bounds each RawSubscription's queue (in reads).
	RawBuffer int
	// SnapshotEvery is how many output bytes trigger a snapshot marker.
	SnapshotEvery int
	// DrainGrace is how long output may keep arriving after the leader exits
	// (descendants holding the PTY) before the exit is journaled.
	DrainGrace time.Duration
	// KillGrace is how long Close waits after SIGTERM before SIGKILL.
	KillGrace time.Duration
	// Ownership tunes input arbitration (ptyevent.Arbitrate).
	Ownership ptyevent.OwnershipConfig

	// beforeOutput, when set (tests), runs on the writer goroutine before each
	// queued read is processed — it lets tests stall the writer.
	beforeOutput func()
}

// DefaultOptions are the runtime defaults (generous; guards fire only at
// extremes).
var DefaultOptions = Options{
	IdleFlush:        100 * time.Millisecond,
	ReadBuffer:       32 << 10,
	QueueDepth:       1024,
	SubscriberBuffer: 4096,
	RawBuffer:        1024,
	SnapshotEvery:    256 << 10,
	DrainGrace:       2 * time.Second,
	KillGrace:        5 * time.Second,
	Ownership:        ptyevent.DefaultOwnership,
}

func (o Options) withDefaults() Options {
	d := DefaultOptions
	if o.IdleFlush <= 0 {
		o.IdleFlush = d.IdleFlush
	}
	if o.ReadBuffer <= 0 {
		o.ReadBuffer = d.ReadBuffer
	}
	if o.QueueDepth <= 0 {
		o.QueueDepth = d.QueueDepth
	}
	if o.SubscriberBuffer <= 0 {
		o.SubscriberBuffer = d.SubscriberBuffer
	}
	if o.RawBuffer <= 0 {
		o.RawBuffer = d.RawBuffer
	}
	if o.SnapshotEvery <= 0 {
		o.SnapshotEvery = d.SnapshotEvery
	}
	if o.DrainGrace <= 0 {
		o.DrainGrace = d.DrainGrace
	}
	if o.KillGrace <= 0 {
		o.KillGrace = d.KillGrace
	}
	if o.Ownership == (ptyevent.OwnershipConfig{}) {
		o.Ownership = d.Ownership
	}
	return o
}

// Config describes the command a Session hosts.
type Config struct {
	// Journal receives the session's events. Its SessionID is the session's.
	// The caller owns it (Close does not close it), so a respawn can continue
	// the same journal under the next epoch.
	Journal *Journal
	// Argv is the native interactive command (argv[0] resolved via PATH).
	Argv []string
	Dir  string
	// Env is the full environment (nil = the daemon's).
	Env []string
	// Cols/Rows is the initial window size (default 80×24).
	Cols, Rows uint16
	// Backend labels the lifecycle events (informational).
	Backend string
	Options Options
	// Adopt, when set, hosts a process this runtime did not start: another
	// process (the PTY holder) owns the master and the child, and this Session
	// is a client of them. Argv/Dir/Env are ignored.
	Adopt *Adoption
}

// ExitStatus is how a hosted leader ended.
type ExitStatus struct {
	Code   int
	Signal string // name of the terminating signal ("" for a normal exit)
	// Unknown: the process is gone but its status was lost (its reaper died).
	Unknown bool
	// Lost: the channel that reports the status broke while the process may
	// still be running. Only an Adoption's Wait returns it.
	Lost bool
}

// Adoption is a process whose PTY master and wait status belong to someone
// else. The Session holds its own dup of the master, so ending the Session
// (Detach) leaves the process and the owner's master untouched: the daemon can
// die, restart or be replaced and the process tree keeps running.
type Adoption struct {
	// Master is this process's own pollable master descriptor.
	Master *os.File
	PID    int
	// Recovered journals lifecycle{recovered} (a re-adoption: same process, new
	// epoch) instead of lifecycle{spawned}.
	Recovered bool
	// Wait blocks until the leader's exit is known. It returns Lost when the
	// channel broke or Abort was called.
	Wait func() ExitStatus
	// Abort unblocks Wait.
	Abort func()
	// Alive reports whether the very same process is still running (identity
	// checked, never a recycled pid). Nil falls back to a signal-0 probe.
	Alive func() bool
}

// Session errors.
var (
	ErrExited    = errors.New("ptyruntime: process has exited")
	ErrAbandoned = errors.New("ptyruntime: automatic input abandoned (human kept the line); re-decide")
	ErrInputRej  = errors.New("ptyruntime: input rejected")
	ErrEmitKind  = errors.New("ptyruntime: event kind is runtime-owned")
	// ErrPreempted: a human typed after the automated input was issued, so the
	// (now stale) input is dropped rather than written over their line.
	ErrPreempted = errors.New("ptyruntime: input preempted by a human; stale automated input dropped")
	// ErrDuplicate: a sequence with the same OnceKey was already issued.
	ErrDuplicate = errors.New("ptyruntime: duplicate once-only input refused")
)

type item struct {
	data    []byte
	dropped int // bytes lost before data (queue overflow)
}

// Session is one native command on a warden-owned PTY. The single writer owns
// the seq: every event is redacted, validated and journaled before any
// subscriber sees it.
type Session struct {
	writer
	cmd    *exec.Cmd // nil for an adopted process
	adopt  *Adoption
	master *os.File
	pid    int
	queue  chan item

	// Guarded by writer.mu.
	lease      ptyevent.Lease
	credPrompt bool
	ended      bool
	exitEv     *ptyevent.Lifecycle

	waitDone   chan struct{} // leader reaped (exit set)
	exit       ExitStatus
	detachCh   chan struct{} // closed by Detach: end the Session, leave the process
	detached   bool
	readDone   chan struct{}
	writerDone chan struct{}
	exited     chan struct{} // exit journaled
	closeOnce  sync.Once

	// enq/done count raw reads handed to the writer and fully applied by it:
	// equal means the journal and screen have caught up with everything the
	// reader published (AttachQuiesced).
	enq, done atomic.Int64

	// Controller lease (guarded by writer.mu): one controller per session.
	ctl       controller
	genSeq    uint64
	lastInput map[string]uint64   // client -> highest applied fenced input id
	once      map[string]struct{} // OnceKeys already issued (guarded by writer.mu)
	// seqMu serializes automatic multi-part writes.  It intentionally does not
	// cover human Input: a human can always preempt a sequence during its gap,
	// and the per-part HumanSeq fence then drops the remaining automatic input.
	seqMu sync.Mutex
}

// Start launches cfg.Argv on a new PTY and starts journaling. The first event
// of the new epoch is lifecycle{spawned}. A journal whose previous epoch never
// ended (the daemon died with the process) first gets lifecycle{crashed} for
// that epoch; any respawn on a non-empty journal then journals marker{reset}
// and a snapshot so clients holding the old screen resynchronize (§6.6).
func Start(cfg Config) (*Session, error) {
	if cfg.Journal == nil {
		return nil, errors.New("ptyruntime: Config.Journal is required")
	}
	if cfg.Adopt == nil && len(cfg.Argv) == 0 {
		return nil, errors.New("ptyruntime: Config.Argv is empty")
	}
	if cfg.Cols == 0 {
		cfg.Cols = 80
	}
	if cfg.Rows == 0 {
		cfg.Rows = 24
	}
	opt := cfg.Options.withDefaults()
	var (
		master *os.File
		cmd    *exec.Cmd
		pid    int
	)
	if ad := cfg.Adopt; ad != nil {
		if ad.Master == nil || ad.PID <= 0 || ad.Wait == nil {
			return nil, errors.New("ptyruntime: Config.Adopt needs a master, a pid and Wait")
		}
		master, pid = ad.Master, ad.PID
		// The process keeps the size its clients last gave it.
		if c, r, err := getWinsize(master); err == nil && c > 0 && r > 0 {
			cfg.Cols, cfg.Rows = c, r
		}
	} else {
		var tty *os.File
		var err error
		master, tty, err = openPTY()
		if err != nil {
			return nil, err
		}
		if err := setWinsize(master, cfg.Cols, cfg.Rows); err != nil {
			master.Close()
			tty.Close()
			return nil, err
		}
		cmd = exec.Command(cfg.Argv[0], cfg.Argv[1:]...)
		cmd.Dir, cmd.Env = cfg.Dir, cfg.Env
		cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
		// A new session with the PTY as controlling terminal: the child leads its
		// own process group (pgid == pid), which is what liveness and teardown
		// signal.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
		if err := cmd.Start(); err != nil {
			master.Close()
			tty.Close()
			return nil, err
		}
		tty.Close() // the child holds the slave; EOF/EIO on master once it is gone
		pid = cmd.Process.Pid
	}

	last := cfg.Journal.Last()
	s := &Session{
		writer: newWriter(opt, cfg.Journal, cfg.Cols, cfg.Rows), cmd: cmd, adopt: cfg.Adopt, master: master, pid: pid,
		queue: make(chan item, opt.QueueDepth), waitDone: make(chan struct{}), readDone: make(chan struct{}),
		writerDone: make(chan struct{}), exited: make(chan struct{}), detachCh: make(chan struct{}),
		lastInput: map[string]uint64{},
	}
	s.genSeq = s.epoch << 32
	s.mu.Lock()
	err := s.open(last, cfg)
	s.mu.Unlock()
	if err != nil {
		if cfg.Adopt == nil {
			_ = unix.Kill(-s.pid, unix.SIGKILL)
			_ = cmd.Wait()
		}
		master.Close()
		return nil, err
	}
	if cfg.Adopt != nil && cfg.Adopt.Recovered {
		s.restoreInputIDs(last)
		s.nudgeRedraw()
	}
	go s.waitLoop()
	go s.readLoop()
	go s.writeLoop()
	return s, nil
}

// openPTY returns a pollable (non-blocking) master so Close interrupts a
// pending Read and deadlines work. pty.Open leaves the master in blocking
// mode (it calls Fd internally), so the fd is dup'ed and re-wrapped.
func openPTY() (master, tty *os.File, err error) {
	ptmx, tty, err := pty.Open()
	if err != nil {
		return nil, nil, err
	}
	fd, err := unix.Dup(int(ptmx.Fd()))
	ptmx.Close()
	if err == nil {
		err = unix.SetNonblock(fd, true)
	}
	if err != nil {
		tty.Close()
		return nil, nil, err
	}
	unix.CloseOnExec(fd)
	return os.NewFile(uintptr(fd), "ptmx"), tty, nil
}

func (s *Session) open(last ptyevent.Event, cfg Config) error {
	recovered := cfg.Adopt != nil && cfg.Adopt.Recovered
	if last.Seq != 0 && !s.j.Ended() && !recovered {
		ev := s.event(ptyevent.KindLifecycle)
		ev.Epoch, ev.MonoNS = last.Epoch, last.MonoNS
		ev.Lifecycle = &ptyevent.Lifecycle{Phase: ptyevent.PhaseCrashed, Backend: cfg.Backend,
			Detail: "runtime lost the process before its exit was journaled"}
		if err := s.commit(ev); err != nil {
			return err
		}
	}
	ev := s.event(ptyevent.KindLifecycle)
	phase := ptyevent.PhaseSpawned
	if recovered {
		phase = ptyevent.PhaseRecovered
	}
	ev.Lifecycle = &ptyevent.Lifecycle{Phase: phase, PID: s.pid, Backend: cfg.Backend,
		Presentation: ptyevent.PresentationDirect}
	if err := s.commit(ev); err != nil {
		return err
	}
	if last.Seq != 0 {
		ev := s.event(ptyevent.KindMarker)
		ev.Marker = &ptyevent.Marker{Type: ptyevent.MarkerReset}
		if err := s.commit(ev); err != nil {
			return err
		}
		return s.snapshot()
	}
	return nil
}

// PID is the process (and process group) id of the hosted command.
func (s *Session) PID() int { return s.pid }

// Done is closed once the process exit has been journaled.
func (s *Session) Done() <-chan struct{} { return s.exited }

// Exit returns the journaled exit (nil until Done).
func (s *Session) Exit() *ptyevent.Lifecycle {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exitEv
}

// Alive reports process-tree liveness: the leader is running, or members of
// its process group (descendants that kept the group) still are.
func (s *Session) Alive() bool {
	select {
	case <-s.waitDone:
	default:
		return true
	}
	err := unix.Kill(-s.pid, 0)
	return err == nil || errors.Is(err, unix.EPERM)
}

// Lease returns the current input-ownership lease (for ptyevent.Revalidate).
func (s *Session) Lease() ptyevent.Lease {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lease
}

// SetCredentialPrompt tells the runtime a credential prompt (API-key entry,
// OAuth paste-code box) is on screen: input is secret until cleared (§5).
func (s *Session) SetCredentialPrompt(on bool) {
	s.mu.Lock()
	s.credPrompt = on
	s.mu.Unlock()
}

// --- input & resize ---------------------------------------------------------

// Write is one input request.
type Write struct {
	Origin   ptyevent.Origin
	ClientID string
	Data     []byte
	// DecisionID / Reason: required (one of) for OriginWarden.
	DecisionID string
	Reason     string
	// FirstDeferredAt is when this write was first deferred (zero on the
	// first try); WriteWait maintains it.
	FirstDeferredAt time.Time
	// Fence, when set, makes the write conditional on the client still holding
	// the controller lease it was issued under, and de-duplicates resent input.
	Fence *Fence
	// FenceHuman drops the write (ErrPreempted) when a human has typed since
	// HumanSeq: the input was decided against a screen/line the human has since
	// changed. HumanSeqOut, when set, receives the lease's HumanSeq at the
	// moment of a successful write (read under the same lock).
	FenceHuman  bool
	HumanSeq    uint64
	HumanSeqOut *uint64
}

// Input arbitrates w against the input lease (human always wins; automatic
// input defers while a human is typing) and, when allowed, writes it to the
// PTY and journals it. The termios is read BEFORE the write: a getpass prompt
// restores echo as soon as it has read the line. Secret input (canonical mode
// with echo off, or a credential prompt on screen) is journaled as its length
// only; other input still goes through Redact. When the verdict is write and
// the error is non-nil, the bytes may already have reached the PTY.
func (s *Session) Input(w Write) (ptyevent.Arbitration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended || s.closed {
		return ptyevent.Arbitration{}, ErrExited
	}
	if err := s.checkFence(w); err != nil {
		return ptyevent.Arbitration{Verdict: ptyevent.VerdictReject, Reason: err.Error()}, err
	}
	if s.pendingGap {
		_ = s.gap()
	}
	if w.FenceHuman && s.lease.HumanSeq != w.HumanSeq {
		return ptyevent.Arbitration{Verdict: ptyevent.VerdictReject, Reason: ErrPreempted.Error()}, ErrPreempted
	}
	req := ptyevent.InputRequest{Origin: w.Origin, ClientID: w.ClientID, At: time.Now(), Seq: s.j.NextSeq(),
		Data: w.Data, DecisionID: w.DecisionID, Reason: w.Reason, FirstDeferredAt: w.FirstDeferredAt}
	lease, arb := ptyevent.Arbitrate(s.lease, req, s.opt.Ownership)
	s.lease = lease
	if arb.Verdict != ptyevent.VerdictWrite {
		return arb, nil
	}
	defer func() { s.lease = ptyevent.Release(s.lease, w.Origin) }()
	if w.HumanSeqOut != nil {
		*w.HumanSeqOut = s.lease.HumanSeq
	}
	mode, merr := ttyMode(s.master)
	secret := s.credPrompt
	if merr != nil {
		secret = true // cannot tell: fail closed, never persist the bytes
	}
	if _, err := s.master.Write(w.Data); err != nil {
		return arb, err
	}
	payload, reds := ptyevent.RedactInput(w.Data, w.Origin, mode, secret)
	payload.ClientID, payload.DecisionID = w.ClientID, w.DecisionID
	if w.Fence != nil && w.Fence.InputID != 0 {
		payload.InputID = w.Fence.InputID
		s.lastInput[w.ClientID] = w.Fence.InputID
	}
	ev := s.event(ptyevent.KindInput)
	ev.Input, ev.Redactions = &payload, reds
	return arb, s.commit(ev)
}

// WriteWait retries Input through deferrals until it is written, abandoned
// (warden input past MaxDefer → ErrAbandoned), rejected, or ctx ends.
func (s *Session) WriteWait(ctx context.Context, w Write) error {
	for {
		arb, err := s.Input(w)
		if err != nil {
			return err
		}
		switch arb.Verdict {
		case ptyevent.VerdictWrite:
			return nil
		case ptyevent.VerdictAbandon:
			return ErrAbandoned
		case ptyevent.VerdictReject:
			return fmt.Errorf("%w: %s", ErrInputRej, arb.Reason)
		}
		if w.FirstDeferredAt.IsZero() {
			w.FirstDeferredAt = time.Now()
		}
		t := time.NewTimer(arb.RetryAfter)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-s.closing:
			t.Stop()
			return ErrClosed
		case <-t.C:
		}
	}
}

// ErrResizeTooLarge refuses a size beyond the screen-model bounds; the
// session keeps its current size.
var ErrResizeTooLarge = errors.New("ptyruntime: terminal size exceeds the supported maximum (1024 columns, 1024 rows, 262144 cells); shrink the window and retry")

// Resize applies a window size (last writer wins) and journals it with its
// origin.
func (s *Session) Resize(cols, rows uint16, origin ptyevent.Origin, clientID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended || s.closed {
		return ErrExited
	}
	if cols == 0 || rows == 0 {
		return ptyevent.ErrResize
	}
	if !FitsScreen(int(cols), int(rows)) {
		return fmt.Errorf("%w: %dx%d", ErrResizeTooLarge, cols, rows)
	}
	if !origin.Valid() {
		return ptyevent.ErrOrigin
	}
	if err := setWinsize(s.master, cols, rows); err != nil {
		return err
	}
	s.screen.Resize(int(cols), int(rows))
	ev := s.event(ptyevent.KindResize)
	ev.Resize = &ptyevent.Resize{Cols: cols, Rows: rows, ClientID: clientID, Origin: origin}
	return s.commit(ev)
}

// Attach registers an interactive client for the live, unredacted byte feed
// and journals lifecycle{attached}.
func (s *Session) Attach(clientID string) (*RawSubscription, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attachLocked(clientID)
}

func (s *Session) attachLocked(clientID string) (*RawSubscription, error) {
	ch := make(chan []byte, s.opt.RawBuffer)
	r := &RawSubscription{C: ch, ClientID: clientID, ch: ch, s: s}
	if s.closed {
		return nil, ErrClosed
	}
	ev := s.event(ptyevent.KindLifecycle)
	ev.Lifecycle = &ptyevent.Lifecycle{Phase: ptyevent.PhaseAttached, ClientID: clientID, Presentation: ptyevent.PresentationDirect}
	if err := s.commit(ev); err != nil {
		return nil, err
	}
	if !s.hub.addRaw(r) {
		return nil, ErrClosed
	}
	return r, nil
}

// quiesceTries bounds how long AttachQuiesced waits for the writer to catch up
// (2ms apart, so ~200ms: longer than the 100ms idle flush): a session printing continuously never quiesces and falls back to
// the plain registration (the stream's resync path covers it).
const quiesceTries = 100

// AttachQuiesced is Attach, registered at a cut where the writer has applied
// every read the reader published: the journal head and the screen model
// include everything before the cut and the raw feed carries everything after
// it, so a snapshot (or replay) followed by the feed neither loses nor repeats
// output. It returns the screen model (StyledString: attributes kept — the local view, never a journal) and the journal head at the cut. Under continuous output the
// cut can't be found within ~100ms and the registration proceeds anyway
// (exact=false): a viewer then relies on resync.
func (s *Session) AttachQuiesced(clientID string) (r *RawSubscription, screen string, head uint64, exact bool, err error) {
	for i := 0; ; i++ {
		s.mu.Lock()
		// Quiet: every read is applied AND nothing is held back (a partial line or
		// UTF-8 tail the redactor releases on its idle flush) — otherwise those
		// bytes would be in neither the replay nor the raw feed.
		quiet := s.enq.Load() == s.done.Load() && !s.in.pending()
		if quiet || i >= quiesceTries {
			exact = quiet
			r, err = s.attachLocked(clientID)
			head, screen = s.j.Last().Seq, s.screen.StyledString() // local authorized view: keeps SGR
			s.mu.Unlock()
			return r, screen, head, exact, err
		}
		s.mu.Unlock()
		time.Sleep(2 * time.Millisecond)
	}
}

func (s *Session) detach(r *RawSubscription) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	ev := s.event(ptyevent.KindLifecycle)
	ev.Lifecycle = &ptyevent.Lifecycle{Phase: ptyevent.PhaseDetached, ClientID: r.ClientID, Presentation: ptyevent.PresentationDirect}
	if r.Lagged() {
		ev.Lifecycle.Detail = "client fell behind"
	}
	if s.commit(ev) == nil && !r.Lagged() {
		_ = s.snapshot()
	}
}

// --- goroutines ------------------------------------------------------------

func (s *Session) waitLoop() {
	if s.adopt == nil {
		err := s.cmd.Wait()
		_ = err
		s.exit = exitFromState(s.cmd.ProcessState)
		close(s.waitDone)
		return
	}
	st := s.adopt.Wait()
	if st.Lost {
		// The owner of the process is gone (or Detach aborted the wait). A
		// detach ends quietly; otherwise watch the process itself: its status
		// is lost, but it must still be reported when it ends — never silently
		// relaunched.
		for s.adopt.alive(s.pid) {
			select {
			case <-s.detachCh:
				return
			case <-time.After(200 * time.Millisecond):
			}
		}
		select {
		case <-s.detachCh:
			return
		default:
		}
		st = ExitStatus{Unknown: true}
	}
	s.exit = st
	close(s.waitDone)
}

func (a *Adoption) alive(pid int) bool {
	if a.Alive != nil {
		return a.Alive()
	}
	err := unix.Kill(pid, 0)
	return err == nil || errors.Is(err, unix.EPERM)
}

func exitFromState(ps *os.ProcessState) ExitStatus {
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return ExitStatus{Signal: unix.SignalName(ws.Signal())}
	}
	return ExitStatus{Code: ps.ExitCode()}
}

// readLoop never blocks on the writer: a full queue drops the read and
// counts the loss for the writer's gap marker. While a loss is pending, a read
// deadline makes sure it is reported even if the PTY then goes quiet.
func (s *Session) readLoop() {
	defer close(s.readDone)
	dropped, deadline := 0, false
	for {
		if dropped > 0 {
			select {
			case s.queue <- item{dropped: dropped}:
				dropped = 0
			default:
			}
		}
		if want := dropped > 0; want || deadline {
			var at time.Time
			if want {
				at = time.Now().Add(s.opt.IdleFlush)
			}
			_ = s.master.SetReadDeadline(at)
			deadline = want
		}
		buf := make([]byte, s.opt.ReadBuffer)
		n, err := s.master.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			s.enq.Add(1) // before the raw publish: a registration that sees enq==done saw everything
			s.hub.raw(chunk)
			select {
			case s.queue <- item{data: chunk, dropped: dropped}:
				dropped = 0
			default:
				dropped += n
				s.done.Add(1) // dropped: the writer will never apply it
			}
		}
		if errors.Is(err, os.ErrDeadlineExceeded) {
			continue
		}
		if err != nil {
			if dropped > 0 {
				select {
				case s.queue <- item{dropped: dropped}: // the writer drains until readDone
				case <-s.detachCh:
				}
			}
			return
		}
	}
}

func (s *Session) writeLoop() {
	defer close(s.writerDone)
	idle := time.NewTimer(time.Hour)
	idle.Stop()
	defer idle.Stop()
	var drain <-chan time.Time
	waitDone, readDone := s.waitDone, s.readDone
	reaped, eof, done := false, false, false
	handle := func(it item) {
		if len(it.data) > 0 {
			defer s.done.Add(1) // after mu is released: "done" means applied to journal and screen
		}
		if s.opt.beforeOutput != nil {
			s.opt.beforeOutput()
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.ended {
			return
		}
		if it.dropped > 0 {
			s.flushOutput()
			_ = s.gap()
		}
		if len(it.data) > 0 {
			if out, reds, ok := s.in.feed(it.data); ok {
				s.commitOutput(out, reds)
			}
		}
		if s.in.pending() {
			idle.Reset(s.opt.IdleFlush)
		}
	}
	for !(done && eof) {
		select {
		case <-s.detachCh:
			// Detach: leave the process alone, keep the journal's epoch open (the
			// next daemon recovers it) and journal nothing more.
			s.mu.Lock()
			if !s.ended {
				s.flushOutput()
			}
			s.mu.Unlock()
			return
		default:
		}
		if reaped && eof && !done {
			s.finish()
			done = true
			continue
		}
		select {
		case it := <-s.queue:
			handle(it)
		case <-idle.C:
			s.mu.Lock()
			if !s.ended {
				s.flushOutput()
			}
			s.mu.Unlock()
		case <-s.detachCh:
		case <-waitDone:
			waitDone, reaped = nil, true
			drain = time.After(s.opt.DrainGrace)
		case <-drain:
			drain = nil
			if !done {
				s.finish() // descendants still hold the PTY; their output is dropped
				done = true
			}
		case <-readDone:
			readDone = nil
			for drained := false; !drained; {
				select {
				case it := <-s.queue:
					handle(it)
				default:
					drained = true
				}
			}
			eof = true
		}
	}
}

// finish flushes held output and journals the process exit.
func (s *Session) finish() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushOutput()
	lc := &ptyevent.Lifecycle{Phase: ptyevent.PhaseExited, PID: s.pid}
	switch {
	case s.exit.Unknown:
		lc.Phase = ptyevent.PhaseCrashed
		lc.Detail = "process ended after its PTY holder was lost; exit status unknown"
	case s.exit.Signal != "":
		lc.Phase, lc.Signal = ptyevent.PhaseSignaled, s.exit.Signal
	default:
		code := s.exit.Code
		lc.ExitCode = &code
	}
	ev := s.event(ptyevent.KindLifecycle)
	ev.Lifecycle = lc
	_ = s.commit(ev)
	s.ended, s.exitEv = true, lc
	close(s.exited)
}

// Detach ends this Session WITHOUT touching the process: the daemon's dup of
// the master is closed, goroutines and subscriptions end, and the journal's
// epoch stays open so the next daemon adopts the very same process (epoch+1,
// lifecycle{recovered}). Only an adopted session can detach — a session whose
// process is its own child has nothing to leave it to.
func (s *Session) Detach() error {
	if s.adopt == nil {
		return s.Close()
	}
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.detached = true
		s.mu.Unlock()
		close(s.detachCh)
		if s.adopt.Abort != nil {
			s.adopt.Abort()
		}
		_ = s.master.Close()
		<-s.writerDone
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		close(s.closing)
		s.hub.close()
		s.wg.Wait()
	})
	return nil
}

// Close tears the session down deterministically: SIGHUP+SIGTERM to the
// process group, SIGKILL after KillGrace, a final SIGKILL sweep of the group,
// then the master is closed, the exit journaled, and every subscription and
// goroutine ended before Close returns. The journal stays open (caller-owned).
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		select {
		case <-s.waitDone:
		default:
			_ = unix.Kill(-s.pid, unix.SIGHUP)
			_ = unix.Kill(-s.pid, unix.SIGTERM)
			t := time.NewTimer(s.opt.KillGrace)
			select {
			case <-s.waitDone:
			case <-t.C:
				_ = unix.Kill(-s.pid, unix.SIGKILL)
				<-s.waitDone
			}
			t.Stop()
		}
		// Stragglers that kept the group (orphaned descendants): kill them and
		// wait, bounded, until init has reaped them.
		for end := time.Now().Add(s.opt.KillGrace); unix.Kill(-s.pid, unix.SIGKILL) == nil && time.Now().Before(end); {
			time.Sleep(5 * time.Millisecond)
		}
		_ = s.master.Close()
		<-s.writerDone
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		close(s.closing)
		s.hub.close()
		s.wg.Wait()
	})
	return nil
}

// WriteSequence writes q as one authority-checked unit through the same input
// lease every client uses. Rules:
//
//   - a human typing always wins: the first part waits for them (operator) or
//     is abandoned (warden past MaxDefer), exactly like WriteWait;
//   - warden sequences are fenced at issue time — if a human typed between the
//     call and the first write the sequence is dropped (ErrPreempted), because
//     what it was decided against is stale;
//   - once the first part is in, any human keystroke before the next part drops
//     the rest (an Enter would submit the human's text mixed with ours);
//   - OnceKey makes the sequence idempotent (ErrDuplicate). The key is released
//     only when nothing was written, so a retry after a clean failure works but
//     a possibly-delivered sequence is never replayed.
func (s *Session) WriteSequence(ctx context.Context, q ptyevent.Sequence) error {
	if q.Origin != ptyevent.OriginOperator && q.Origin != ptyevent.OriginWarden {
		return fmt.Errorf("%w: sequence origin %q", ErrInputRej, q.Origin)
	}
	if len(q.Parts) == 0 {
		return nil
	}
	// Take this snapshot before waiting for another sequence.  In particular, a
	// warden decision is about the screen at call time, not the later time when
	// it reaches the front of the automatic-input queue.
	s.mu.Lock()
	issued := s.lease.HumanSeq
	if q.OnceKey != "" {
		if _, dup := s.once[q.OnceKey]; dup {
			s.mu.Unlock()
			return ErrDuplicate
		}
		if s.once == nil {
			s.once = map[string]struct{}{}
		}
		s.once[q.OnceKey] = struct{}{}
	}
	s.mu.Unlock()

	// Do not hold s.mu here: human input must remain able to preempt a queued or
	// in-progress automatic sequence.  This only excludes other automatic
	// sequences, so their paste/submit parts cannot interleave.
	s.seqMu.Lock()
	defer s.seqMu.Unlock()
	written := false
	release := func() {
		if q.OnceKey != "" && !written {
			s.mu.Lock()
			delete(s.once, q.OnceKey)
			s.mu.Unlock()
		}
	}
	defer release()

	base := issued
	for i, p := range q.Parts {
		w := Write{Origin: q.Origin, Data: p.Data, DecisionID: q.DecisionID, Reason: q.Reason}
		switch {
		case i > 0:
			w.FenceHuman, w.HumanSeq = true, base
		case q.Origin == ptyevent.OriginWarden:
			w.FenceHuman, w.HumanSeq = true, base
		}
		if i == 0 {
			// Capture the human sequence atomically with the first write, so an
			// operator send that waited out a human fences from the moment it landed.
			w.HumanSeqOut = &base
		}
		if err := s.WriteWait(ctx, w); err != nil {
			return err
		}
		written = true
		if p.Gap > 0 && i < len(q.Parts)-1 {
			t := time.NewTimer(p.Gap)
			select {
			case <-ctx.Done():
				t.Stop()
				return ctx.Err()
			case <-s.closing:
				t.Stop()
				return ErrClosed
			case <-t.C:
			}
		}
	}
	return nil
}
